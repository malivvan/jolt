//go:build wazero && !(js && wasm)

package js

import (
	"fmt"
	"io"
	"io/fs"
	"runtime"
	"time"

	"github.com/MoeForge/moejs"
)

// SourceMode says how Config.Source is compiled.
type SourceMode int

const (
	// ModeAuto compiles an ES module, or TypeScript when the file extension
	// is .ts/.mts/.cts/.tsx. This is the zero value.
	ModeAuto SourceMode = iota
	// ModeModule compiles an ES module (or TypeScript with Config.TypeScript).
	ModeModule
	// ModeScript compiles a classic script. Its var/function declarations
	// become global properties; it has no exports.
	ModeScript
)

func (m SourceMode) String() string {
	switch m {
	case ModeAuto:
		return "auto"
	case ModeModule:
		return "module"
	case ModeScript:
		return "script"
	default:
		return fmt.Sprintf("SourceMode(%d)", int(m))
	}
}

// DecodeOptions bounds a result decoded into a Go value: MaxBytes its JSON
// text in bytes, MaxNodes the values in it. A zero field sets no limit. It is
// moejs's DecodeOptions.
type DecodeOptions = moejs.DecodeOptions

// Config describes one JavaScript program and the runtimes that execute it.
//
// The zero value is usable as long as Source or SourceFile is set: the program
// is an ES module, the event loop is installed with timers, there is no file
// system, no network, no console output and no memory limit, and returned
// promises are awaited.
//
// Config is copied by New. Maps and slices are read but not modified.
type Config struct {
	// ------------------------------------------------------------------
	// Program source

	// Name is the program's name in errors and stack traces. Empty means the
	// base name of SourceFile, or "canopy.js".
	Name string
	// Source is the JavaScript or TypeScript source. It and SourceFile are
	// mutually exclusive.
	Source string
	// SourceFile is a host path to read the program from. Imports are
	// resolved relative to its directory unless ModuleFS or Loader is set.
	SourceFile string
	// TypeScript compiles the source as TypeScript, as moejs.CompileTS does.
	// ModeAuto also turns it on for a .ts/.mts/.cts/.tsx name.
	TypeScript bool
	// Mode selects module or classic-script compilation. See SourceMode.
	Mode SourceMode

	// ------------------------------------------------------------------
	// Engine settings (moejs.Options)

	// TimeZone is the local time zone of Date; nil means time.Local.
	TimeZone *time.Location
	// Locale is the default locale of Intl and toLocaleString, a BCP 47 tag
	// such as "de-DE". Empty means the engine's default ("en-US").
	Locale string
	// MemoryLimit caps what one runtime's JavaScript may retain and
	// allocate, in bytes. Zero means no limit.
	MemoryLimit int64
	// MutableIntrinsics gives every runtime its own mutable copy of the
	// builtins. By default they are shared, frozen and read-only.
	MutableIntrinsics bool
	// MaxDynamicSource is the maximum length in bytes of source text that
	// eval, new Function and Realm.EvalScript may compile. Zero means the
	// engine default (1 MiB); a negative value means no limit.
	MaxDynamicSource int
	// DisableDynamicCode makes eval and the Function constructors throw
	// instead of compiling. Compile, CompileScript and Eval are not affected.
	DisableDynamicCode bool

	// ------------------------------------------------------------------
	// Compilation and execution

	// CompileTimeout bounds compiling the program and its imports. Zero
	// means no bound.
	CompileTimeout time.Duration
	// DefaultTimeout is applied to Call, Eval and Executor.Call when their
	// context has no earlier deadline. Zero means no default.
	DefaultTimeout time.Duration
	// DisableAwaitPromises makes a call return the JavaScript promise itself
	// instead of waiting for it to settle. By default a call awaits a returned
	// promise on the event loop, and a rejection is returned as an error.
	DisableAwaitPromises bool
	// Decode bounds results decoded into Go (Call's return value and
	// CallInto). A zero field sets no limit.
	Decode moejs.DecodeOptions

	// ------------------------------------------------------------------
	// Event loop

	// OnLoopError receives errors thrown by timers, immediates and host
	// tasks the event loop runs. When nil, such an error stops the loop and
	// is returned from the call that was running it. An interrupt is never
	// passed here.
	OnLoopError func(error)
	// ResetMemoryPerTask resets the memory-limit budget before each timer,
	// immediate and task instead of once per call. See moejs eventloop.Options.
	ResetMemoryPerTask bool
	// OnIdle is called when the loop is about to sleep. It may block.
	OnIdle func()
	// OnWake is called when the loop wakes. A non-nil error stops the loop.
	OnWake func() error
	// OnUnhandledRejection is called when a promise is rejected with no
	// handler by the time a call ends. The callback runs on the runtime's
	// goroutine, outside any JavaScript call.
	OnUnhandledRejection func(*JSError)

	// ------------------------------------------------------------------
	// Composable features

	// FS is the file system feature. Nil means NoFS: importing node:fs fails.
	// HostFS installs node:fs and node:fs/promises over one host directory.
	FS FS
	// Network is the network feature. Nil means NoNetwork: no fetch, URL or
	// Streams globals are installed. HostNetwork installs the Web APIs;
	// with a nil Client it installs them without network access.
	Network Network
	// Features are extra features installed in every runtime, after FS and
	// Network. A later feature's globals win; a feature's modules take
	// precedence if it implements ImporterProvider/ResolverProvider.
	Features []Feature
	// WebAssembly configures the WebAssembly JavaScript API. The API is
	// installed when canopy is built with the "canopy_wasm" build tag and
	// this option is not disabled. See WebAssemblySupported.
	WebAssembly WebAssemblyOptions

	// ------------------------------------------------------------------
	// Modules and imports

	// Modules maps import specifiers to module source. It is consulted
	// before Loader, ModuleFS and the host file system.
	Modules map[string]string
	// Loader loads module sources for import specifiers. Nil means the
	// built-in loader.
	Loader ModuleLoader
	// ModuleFS is the io/fs file system module sources are read from. It is
	// useful with embed.FS. Nil means the host file system, if SourceFile is
	// set, or no host access for inline source.
	ModuleFS fs.FS
	// ModuleRoot is the directory inside ModuleFS that module names are
	// relative to. Empty means the root of ModuleFS.
	ModuleRoot string
	// Resolve, when not nil, is consulted before the built-in resolver. It
	// may return ErrModuleNotFound to fall through.
	Resolve moejs.Resolver
	// Importer replaces the importer used for import() and import.meta.
	// Normally canopy builds one from the resolver and the features.
	Importer *moejs.Importer

	// ------------------------------------------------------------------
	// Exposed values

	// Globals are installed as global variables in every runtime before the
	// program loads. Values may be funcs, maps, slices, structs or *Value.
	Globals map[string]any
	// Functions are installed as global host functions in every runtime. They
	// are registered after Globals, with SetFunc semantics; each value must be
	// a func.
	Functions map[string]any
	// Console receives console.log and friends. Nil discards output unless
	// DisableConsole is set. The console is installed unless DisableConsole.
	Console io.Writer
	// DisableConsole does not install a console global.
	DisableConsole bool

	// ------------------------------------------------------------------
	// Executor pool

	// PoolSize bounds how many runtimes run at once. Zero means
	// runtime.GOMAXPROCS(0). It must be positive after defaults are applied.
	PoolSize int
	// MaxIdle bounds how many idle runtimes are kept for reuse. Zero means
	// PoolSize. Set NoReuse to keep none.
	MaxIdle int
	// NoReuse closes a runtime instead of returning it to the pool after a
	// call. It gives every call a fresh JavaScript state at the cost of
	// creating a runtime each time.
	NoReuse bool

	// ------------------------------------------------------------------
	// Hooks

	// OnRuntimeReady runs after a new runtime has loaded the program and
	// installed its features. It runs on the runtime's goroutine, before the
	// runtime is used. Returning an error closes the runtime.
	OnRuntimeReady func(*Runtime) error
	// OnRuntimeClose runs before a runtime is closed, after JavaScript has
	// stopped.
	OnRuntimeClose func(*Runtime) error
}

// withDefaults returns a copy of c with zero values replaced by their
// defaults and the selected features in install order.
func (c Config) withDefaults() (Config, error) {
	if c.Source != "" && c.SourceFile != "" {
		return Config{}, fmt.Errorf("canopy: Config.Source and Config.SourceFile are both set")
	}
	if c.Source == "" && c.SourceFile == "" {
		return Config{}, ErrNoSource
	}
	if c.Mode < ModeAuto || c.Mode > ModeScript {
		return Config{}, fmt.Errorf("canopy: invalid Config.Mode %d", c.Mode)
	}
	if c.MemoryLimit < 0 {
		return Config{}, fmt.Errorf("canopy: Config.MemoryLimit is negative")
	}
	if c.PoolSize < 0 {
		return Config{}, fmt.Errorf("canopy: Config.PoolSize is negative")
	}
	if c.MaxIdle < 0 {
		return Config{}, fmt.Errorf("canopy: Config.MaxIdle is negative")
	}
	if c.PoolSize == 0 {
		c.PoolSize = runtime.GOMAXPROCS(0)
		if c.PoolSize < 1 {
			c.PoolSize = 1
		}
	}
	if c.MaxIdle == 0 {
		c.MaxIdle = c.PoolSize
	}
	if c.MaxIdle > c.PoolSize {
		c.MaxIdle = c.PoolSize
	}
	if c.Name == "" {
		if c.SourceFile != "" {
			c.Name = baseName(c.SourceFile)
		} else {
			c.Name = "canopy.js"
		}
	}
	c.Modules = cloneStringMap(c.Modules)
	c.Globals = cloneAnyMap(c.Globals)
	c.Functions = cloneAnyMap(c.Functions)
	c.Features = cloneSlice(c.Features)
	return c, nil
}

// features returns FS, Network and Config.Features in install order.
func (c Config) allFeatures() []Feature {
	out := make([]Feature, 0, len(c.Features)+2)
	if c.FS != nil {
		out = append(out, c.FS)
	}
	if c.Network != nil {
		out = append(out, c.Network)
	}
	return append(out, c.Features...)
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneSlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	return append([]T(nil), s...)
}

// baseName returns the last path element, accepting both separators.
func baseName(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' || name[i] == '\\' {
			return name[i+1:]
		}
	}
	return name
}

// isTypeScriptName reports whether name has a TypeScript extension.
func isTypeScriptName(name string) bool {
	return hasExtension(name, ".ts") || hasExtension(name, ".mts") ||
		hasExtension(name, ".cts") || hasExtension(name, ".tsx")
}

func hasExtension(name, ext string) bool {
	return len(name) >= len(ext) && equalFold(name[len(name)-len(ext):], ext)
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
