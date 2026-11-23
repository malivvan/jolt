package js

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
)

// ModuleSource is one module's source, returned by a ModuleLoader.
type ModuleSource struct {
	// Name is the module's canonical name, used in stack traces and as the
	// referrer for its own imports. Empty lets js use the resolved path.
	Name       string
	Source     string
	TypeScript bool
}

// ModuleLoader loads module source for an import specifier. Load is called
// once per (referrer, specifier) while the program is compiled; it may be
// called concurrently.
//
// Return ErrModuleNotFound (possibly wrapped) to let the next loader try.
type ModuleLoader interface {
	Load(referrer, specifier string) (ModuleSource, error)
}

// ModuleLoaderFunc adapts a function to ModuleLoader.
type ModuleLoaderFunc func(referrer, specifier string) (ModuleSource, error)

// Load calls f.
func (f ModuleLoaderFunc) Load(referrer, specifier string) (ModuleSource, error) {
	return f(referrer, specifier)
}

// SourceSet is a ModuleLoader over a map of module names to source. Exact
// specifier matches win; otherwise the specifier is resolved relative to the
// referrer and common extensions and index files are tried.
type SourceSet map[string]string

// Load implements ModuleLoader.
func (s SourceSet) Load(referrer, specifier string) (ModuleSource, error) {
	if src, ok := s[specifier]; ok {
		return ModuleSource{Name: specifier, Source: src, TypeScript: isTypeScriptName(specifier)}, nil
	}
	for _, name := range importCandidates(referrer, specifier) {
		if src, ok := s[name]; ok {
			return ModuleSource{Name: name, Source: src, TypeScript: isTypeScriptName(name)}, nil
		}
	}
	return ModuleSource{}, fmt.Errorf("%w: %q (imported from %q)", ErrModuleNotFound, specifier, referrer)
}

// FSLoader loads module source from an io/fs file system, which may be an
// embed.FS. Names are slash paths relative to Root. Paths outside Root are
// refused.
type FSLoader struct {
	FS   fs.FS
	Root string
}

// NewFSLoader returns a loader over fsys, with module names relative to root.
func NewFSLoader(fsys fs.FS, root string) *FSLoader {
	return &FSLoader{FS: fsys, Root: root}
}

// Load implements ModuleLoader.
func (l *FSLoader) Load(referrer, specifier string) (ModuleSource, error) {
	if l == nil || l.FS == nil {
		return ModuleSource{}, fmt.Errorf("%w: no io/fs file system", ErrModuleNotFound)
	}
	root := strings.Trim(path.Clean(strings.ReplaceAll(l.Root, "\\", "/")), "/")
	// The referrer may already carry the root: Config.Name is passed as the
	// referrer for the entry module, and a caller may name a module by its
	// full path. Candidates are root-relative, so strip the prefix first;
	// otherwise the root would be applied twice.
	ref := strings.ReplaceAll(referrer, "\\", "/")
	if root != "" && root != "." {
		if ref == root {
			ref = ""
		} else if strings.HasPrefix(ref, root+"/") {
			ref = strings.TrimPrefix(ref, root+"/")
		}
	}
	for _, name := range importCandidates(ref, specifier) {
		if !validModuleName(name) {
			continue
		}
		p := name
		if root != "" && root != "." {
			p = path.Join(root, name)
		}
		if !fs.ValidPath(p) {
			continue
		}
		data, err := fs.ReadFile(l.FS, p)
		if err == nil {
			return ModuleSource{Name: name, Source: string(data), TypeScript: isTypeScriptName(name)}, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return ModuleSource{}, fmt.Errorf("js: read module %q: %w", p, err)
		}
	}
	return ModuleSource{}, fmt.Errorf("%w: %q (imported from %q)", ErrModuleNotFound, specifier, referrer)
}

// hostLoader loads module source from the host file system below one
// directory. It refuses paths that leave that directory.
type hostLoader struct {
	dir string
}

func (l hostLoader) Load(referrer, specifier string) (ModuleSource, error) {
	dir, err := filepath.Abs(l.dir)
	if err != nil {
		return ModuleSource{}, err
	}
	for _, name := range importCandidates(referrer, specifier) {
		if !validModuleName(name) {
			continue
		}
		// OpenInRoot confines both ".." and symbolic links to dir; a lexical
		// filepath.Rel check alone would follow a link that points outside.
		f, err := os.OpenInRoot(dir, filepath.FromSlash(name))
		if err == nil {
			data, readErr := io.ReadAll(f)
			if closeErr := f.Close(); readErr == nil {
				readErr = closeErr
			}
			if readErr != nil {
				return ModuleSource{}, fmt.Errorf("js: read module %q: %w", name, readErr)
			}
			return ModuleSource{Name: name, Source: string(data), TypeScript: isTypeScriptName(name)}, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return ModuleSource{}, fmt.Errorf("js: read module %q: %w", name, err)
		}
	}
	return ModuleSource{}, fmt.Errorf("%w: %q (imported from %q)", ErrModuleNotFound, specifier, referrer)
}

// importCandidates returns the canonical names to try for specifier, in
// order: exact (and relative to referrer when applicable), then common
// module extensions and index files.
func importCandidates(referrer, specifier string) []string {
	spec := strings.TrimSpace(specifier)
	if spec == "" {
		return nil
	}
	ref := strings.ReplaceAll(referrer, "\\", "/")
	if ref == "" {
		ref = "." + "/"
	}
	var roots []string
	switch {
	case spec == "." || spec == ".." || strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
		roots = append(roots, path.Join(path.Dir(ref), spec))
	case strings.HasPrefix(spec, "/"):
		roots = append(roots, path.Clean(strings.TrimPrefix(spec, "/")))
	default:
		roots = append(roots, path.Clean(spec))
		if ref != "" && path.Dir(ref) != "." {
			roots = append(roots, path.Join(path.Dir(ref), spec))
		}
	}

	seen := make(map[string]bool, len(roots)*6)
	out := make([]string, 0, len(roots)*6)
	add := func(name string) {
		name = path.Clean(strings.ReplaceAll(name, "\\", "/"))
		if name == "." || name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, root := range roots {
		add(root)
		if path.Ext(root) == "" {
			for _, ext := range []string{".js", ".mjs", ".cjs", ".ts", ".mts", ".cts"} {
				add(root + ext)
			}
			for _, idx := range []string{"index.js", "index.mjs", "index.cjs", "index.ts", "index.mts"} {
				add(path.Join(root, idx))
			}
		}
	}
	return out
}

func validModuleName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.HasPrefix(name, "../") && !strings.HasPrefix(name, "/")
}

// compileModule compiles name once and caches it. Imports are resolved
// lazily by resolver, through compileModule too, so a module shared by two
// importers is the same *moejs.Module.
func (p *program) compileModule(name, source string, ts bool) (*moejs.Module, error) {
	p.mu.Lock()
	m := p.modules[name]
	p.mu.Unlock()
	if m != nil {
		return m, nil
	}

	ctx, cancel := p.compileContext()
	defer cancel()
	m, err := moejs.CompileWith(name, source, moejs.CompileOptions{
		TypeScript: ts,
		Context:    ctx,
	})
	if err != nil {
		return nil, wrap("compile", name, err)
	}

	p.mu.Lock()
	if existing := p.modules[name]; existing != nil {
		m = existing
	} else {
		p.modules[name] = m
	}
	p.mu.Unlock()
	return m, nil
}

func (p *program) compileContext() (context.Context, context.CancelFunc) {
	if p.cfg.CompileTimeout > 0 {
		return context.WithTimeout(context.Background(), p.cfg.CompileTimeout)
	}
	return context.Background(), func() {}
}

// loadSource asks the configured loaders for specifier. The order is:
// Config.Modules, Config.Loader, Config.ModuleFS, then the host directory of
// Config.SourceFile.
func (p *program) loadSource(referrer, specifier string) (ModuleSource, error) {
	if p.cfg.Modules != nil {
		if src, ok := p.cfg.Modules[specifier]; ok {
			return ModuleSource{Name: specifier, Source: src, TypeScript: isTypeScriptName(specifier)}, nil
		}
		for _, name := range importCandidates(referrer, specifier) {
			if src, ok := p.cfg.Modules[name]; ok {
				return ModuleSource{Name: name, Source: src, TypeScript: isTypeScriptName(name)}, nil
			}
		}
	}
	if p.cfg.Loader != nil {
		src, err := p.cfg.Loader.Load(referrer, specifier)
		if err == nil {
			return p.fillSource(src, specifier), nil
		}
		if !errors.Is(err, ErrModuleNotFound) {
			return ModuleSource{}, err
		}
	}
	if p.cfg.ModuleFS != nil {
		return NewFSLoader(p.cfg.ModuleFS, p.cfg.ModuleRoot).Load(referrer, specifier)
	}
	if p.cfg.SourceFile != "" {
		return hostLoader{dir: filepath.Dir(p.cfg.SourceFile)}.Load(referrer, specifier)
	}
	return ModuleSource{}, fmt.Errorf("%w: %q (imported from %q)", ErrModuleNotFound, specifier, referrer)
}

func (p *program) fillSource(src ModuleSource, specifier string) ModuleSource {
	if src.Name == "" {
		src.Name = specifier
	}
	if !src.TypeScript && isTypeScriptName(src.Name) {
		src.TypeScript = true
	}
	return src
}

// resolve is the built-in moejs.Resolver.
func (p *program) resolve(referrer moejs.Referrer, specifier string) (*moejs.Module, error) {
	refName := p.entryName
	if referrer != nil {
		refName = referrer.Name()
	}
	src, err := p.loadSource(refName, specifier)
	if err != nil {
		return nil, err
	}
	src = p.fillSource(src, specifier)
	return p.compileModule(src.Name, src.Source, src.TypeScript)
}

// program is a compiled source graph plus the features that every runtime
// installs. It is immutable after construction.
type program struct {
	cfg         Config
	features    []Feature
	entryName   string
	entryModule *moejs.Module
	entryScript *moejs.Script
	resolver    moejs.Resolver
	importer    *moejs.Importer
	console     *consoleWriter

	mu      sync.Mutex
	modules map[string]*moejs.Module
}

func newProgram(cfg Config) (*program, error) {
	p := &program{
		cfg:      cfg,
		features: cfg.allFeatures(),
		modules:  make(map[string]*moejs.Module),
	}
	p.entryName = cfg.Name
	if !cfg.DisableConsole {
		p.console = &consoleWriter{w: cfg.Console}
	}

	resolver := moejs.Resolver(p.resolve)
	if cfg.Resolve != nil {
		custom := cfg.Resolve
		resolver = func(ref moejs.Referrer, spec string) (*moejs.Module, error) {
			m, err := custom(ref, spec)
			if m != nil || (err != nil && !errors.Is(err, ErrModuleNotFound)) {
				return m, err
			}
			return p.resolve(ref, spec)
		}
	}
	// A feature's resolver wraps the ones after it, so the first feature wins
	// when two features know the same specifier.
	for i := len(p.features) - 1; i >= 0; i-- {
		if p.features[i] == nil {
			continue
		}
		if rp, ok := p.features[i].(ResolverProvider); ok {
			resolver = rp.Resolver(resolver)
		}
	}
	p.resolver = resolver

	imp := cfg.Importer
	if imp == nil {
		imp = &moejs.Importer{Resolve: resolver}
	}
	for i := len(p.features) - 1; i >= 0; i-- {
		if p.features[i] == nil {
			continue
		}
		if ip, ok := p.features[i].(ImporterProvider); ok {
			imp = ip.Importer(imp)
		}
	}
	p.importer = imp

	ts := cfg.TypeScript
	if !ts && isTypeScriptName(cfg.Name) {
		ts = true
	}

	switch cfg.Mode {
	case ModeScript:
		src, err := p.entrySource()
		if err != nil {
			return nil, err
		}
		script, err := compileScript(cfg.Name, src, ts, cfg.CompileTimeout)
		if err != nil {
			return nil, err
		}
		p.entryScript = script
	default:
		src, err := p.entrySource()
		if err != nil {
			return nil, err
		}
		entry, err := p.compileModule(cfg.Name, src, ts)
		if err != nil {
			return nil, err
		}
		linked, err := moejs.Link(entry, resolver)
		if err != nil {
			return nil, wrap("link", cfg.Name, err)
		}
		p.entryModule = linked
		p.modules[cfg.Name] = linked
	}
	return p, nil
}

func (p *program) entrySource() (string, error) {
	if p.cfg.Source != "" {
		return p.cfg.Source, nil
	}
	data, err := os.ReadFile(p.cfg.SourceFile)
	if err != nil {
		return "", fmt.Errorf("js: read source file %q: %w", p.cfg.SourceFile, err)
	}
	return string(data), nil
}

func compileScript(name, source string, ts bool, timeout time.Duration) (*moejs.Script, error) {
	var ctx context.Context
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	defer cancel()
	s, err := moejs.CompileScriptWith(name, source, moejs.CompileOptions{TypeScript: ts, Context: ctx})
	if err != nil {
		return nil, wrap("compile", name, err)
	}
	return s, nil
}

// newRuntime creates one initialized runtime.
func (p *program) newRuntime() (*Runtime, error) {
	cfg := p.cfg
	raw := moejs.NewRuntime(moejs.Options{
		MutableIntrinsics:  cfg.MutableIntrinsics,
		TimeZone:           cfg.TimeZone,
		Locale:             cfg.Locale,
		Importer:           p.importer,
		MaxDynamicSource:   cfg.MaxDynamicSource,
		DisableDynamicCode: cfg.DisableDynamicCode,
		MemoryLimit:        cfg.MemoryLimit,
	})
	loop, err := eventloop.New(raw, eventloop.Options{
		OnError:            cfg.OnLoopError,
		ResetMemoryPerTask: cfg.ResetMemoryPerTask,
		OnIdle:             cfg.OnIdle,
		OnWake:             cfg.OnWake,
	})
	if err != nil {
		return nil, wrap("init", p.entryName, err)
	}

	r := &Runtime{prog: p, cfg: cfg, raw: raw, loop: loop}
	if cfg.OnUnhandledRejection != nil {
		raw.SetPromiseRejectionTracker(func(v moejs.Value, op moejs.PromiseRejectionOperation) {
			if op != moejs.PromiseRejectionReject {
				return
			}
			state, reason, ok := moejs.PromiseResult(v)
			if !ok || state != moejs.PromiseRejected {
				return
			}
			var js *JSError
			if err := r.rejectionError(reason); errors.As(err, &js) {
				cfg.OnUnhandledRejection(js)
			}
		})
	}

	env := &Env{r: r, cfg: cfg}
	for _, f := range p.features {
		if f == nil {
			continue
		}
		cleanup, err := f.Install(env)
		if err != nil {
			r.abort()
			return nil, wrap("install", fmt.Sprintf("%T", f), err)
		}
		if cleanup != nil {
			r.cleanups = append(r.cleanups, cleanup)
		}
	}
	if p.console != nil {
		if _, err := (consoleFeature{cw: p.console}).Install(env); err != nil {
			r.abort()
			return nil, wrap("install", "console", err)
		}
	}

	if !cfg.WebAssembly.Disable {
		cleanup, err := installWebAssembly(env, cfg.WebAssembly)
		if err != nil {
			r.abort()
			return nil, wrap("install", "WebAssembly", err)
		}
		if cleanup != nil {
			r.cleanups = append(r.cleanups, cleanup)
		}
	}

	// Config globals and functions win over feature globals of the same name.
	for name, v := range cfg.Globals {
		if err := r.Set(name, v); err != nil {
			r.abort()
			return nil, err
		}
	}
	for name, fn := range cfg.Functions {
		if err := r.SetFunc(name, fn); err != nil {
			r.abort()
			return nil, err
		}
	}

	if p.entryScript != nil {
		if _, err := raw.RunScript(p.entryScript); err != nil {
			r.abort()
			return nil, wrap("load", p.entryName, err)
		}
	} else {
		err := raw.Load(p.entryModule)
		if err != nil && !errors.Is(err, moejs.ErrModulePending) {
			r.abort()
			return nil, wrap("load", p.entryName, err)
		}
	}

	if cfg.OnRuntimeReady != nil {
		if err := cfg.OnRuntimeReady(r); err != nil {
			r.abort()
			return nil, err
		}
	}
	return r, nil
}

// abort releases a runtime that failed to initialize. It skips the user's
// OnRuntimeClose hook, which is only for runtimes that were handed out.
func (r *Runtime) abort() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		_ = r.cleanups[i]()
	}
	r.cleanups = nil
	r.loop.Terminate()
	r.raw.ClearInterrupt()
	r.closed.Store(true)
}

// sortedKeys is used by value conversion for stable object shapes.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
