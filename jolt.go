// Package jolt is a developer-focused JavaScript executor built on the
// moejs engine.
//
// jolt wraps moejs with the pieces a Go program usually ends up writing
// itself:
//
//   - a Config that gathers the engine, compiler, event-loop, sandbox and
//     pool settings in one place;
//   - SetFunc, SetNamespace and Set, which expose Go functions and values to
//     JavaScript without writing moejs.NativeFunc plumbing;
//   - Call, CallInto, Get and Func, which reach functions and values inside a
//     loaded module or script and convert them back to Go;
//   - composable features: a file system, a network, or anything else that
//     implements Feature, installed per runtime;
//   - an Executor that compiles the program once and serves concurrent calls
//     from a pool of runtimes.
//
// A module is compiled and linked once. Every runtime created by an Executor
// loads the same *moejs.Module, which is immutable and safe to share.
//
// # Quick start
//
//	source := `
//	export function greet(name) {
//	  return utils.prefix + " " + name.trim();
//	}
//	`
//	exec, err := jolt.New(jolt.Config{
//		Name:   "greet.js",
//		Source: source,
//		Globals: map[string]any{
//			"utils": map[string]any{"prefix": "hello"},
//		},
//	})
//	if err != nil {
//		panic(err)
//	}
//	defer exec.Close()
//
//	out, err := exec.Call(context.Background(), "greet", "  world  ")
//	// out == "hello world"
//
// # Exposing Go
//
// SetFunc registers a Go function. The wrapper understands any function
// signature whose arguments and results are JSON-shaped Go values:
//
//	rt.SetFunc("sum", func(a, b int) int { return a + b })
//	rt.SetFunc("find", func(ctx context.Context, q Query) ([]Result, error) {
//		return db.Find(ctx, q)
//	})
//
// A parameter of type context.Context receives the context of the running
// call, so a JavaScript invocation is cancelled when the Go caller's context
// ends. A parameter of type moejs.Value receives the raw engine value, and
// *Value receives a jolt handle. A parameter of a Go func type receives the
// JavaScript function as a callable Go value, so a host function can take a
// callback. A function may be variadic and may return (T), (T, error) or
// error. A Go panic inside a registered function becomes a *PanicError thrown
// back to JavaScript; JavaScript may catch it as an Error.
//
// SetNamespace exposes a map as one global object, converting nested maps,
// slices, funcs and *Value handles, and Set exposes a single value. Structs,
// maps and slices without functions go through moejs's FromGo conversion
// (JSON-shaped structs are marshalled as JSON).
//
// # Accessing JavaScript
//
// Call resolves a dotted path such as "protocols.openai.decode" in the loaded
// module's exports first and then in the global object. It converts each Go
// argument for JavaScript, invokes the function, and by default awaits a
// returned promise using the runtime's event loop. CallInto decodes the
// result into a Go value with moejs's JSON-compatible decoder.
//
//	req, err := exec.Call(context.Background(), "buildRequest", task)
//	var body Request
//	err = exec.CallInto(ctx, "buildRequest", []any{task}, &body)
//
// A Runtime leases one runtime. Its Value and Function handles stay valid
// until Close; use Get, Func and Value.Call when the Go side drives several
// calls against the same JavaScript state.
//
// # Composable runtimes
//
// A runtime is assembled from features. Config.FS selects a file system
// (HostFS installs node:fs/node:fs/promises over one host directory, NoFS
// installs nothing). Config.Network selects a network (HostNetwork installs
// fetch and the rest of the Web APIs through a host http.Client, NoNetwork
// installs nothing). Config.Features accepts any Feature, so a host can add
// its own globals, modules or sandbox policies:
//
//	type metrics struct{ sink io.Writer }
//
//	func (m metrics) Install(env *jolt.Env) (func() error, error) {
//		return nil, env.SetFunc("recordMetric", m.record)
//	}
//
//	exec, err := jolt.New(jolt.Config{
//		Source:  source,
//		Network: &jolt.HostNetwork{Client: client, Allow: jolt.AllowHosts("api.example.com")},
//		Features: []jolt.Feature{metrics{sink}},
//	})
//
// # WebAssembly
//
// The browser WebAssembly JavaScript API is installed by default on top of
// wazero. Build with the "nowasm" tag to leave it out:
//
//	go build -tags nowasm ./...
//
// The opt-out tag is not named "wasm" because Go reserves that name for
// GOARCH=wasm; enabling it on another architecture makes the standard library
// select its wasm files and fail to compile. The API is also left out when
// building for GOARCH=wasm. WebAssemblySupported reports whether the binary
// has the feature.
//
// The manager is configured like a browser: WebAssembly Core 2.0 features, a
// 4 GiB memory ceiling, and no automatic "_start" unless
// Config.WebAssembly.WASI.RunStart is set. The WASI host module
// ("wasi_snapshot_preview1") is loaded automatically, so WASI-targeting
// modules instantiate without the JavaScript caller supplying WASI imports;
// Config.WebAssembly.WASI.Disable turns that off. A guest that imports WASI
// gets its environment from Config.WebAssembly.WASI: Args, Env, FS, Stdin,
// Stdout, Stderr and Rand. Defaults are deliberately sandboxed: no arguments,
// no environment, no host file system, no stdio, and deterministic
// randomness.
//
// Config.WebAssembly.Disable leaves the global out;
// Config.WebAssembly.MemoryLimitPages lowers the memory ceiling. JavaScript
// gets Module, Instance, Memory, Global, Table, compile, instantiate, validate,
// the streaming variants, the Error classes, and Module.exports/imports/
// customSections. Function, memory and global imports and exports are bridged
// to wazero; v128/reference values, shared memories and table element access
// are not supported by wazero's public API.
//
// # Concurrency and lifetime
//
// An Executor is safe for concurrent use. Its PoolSize bounds how many
// runtimes run JavaScript at once; each call takes an idle runtime or creates
// one. A Runtime is not safe for concurrent use: it owns one moejs runtime,
// which must be used from one goroutine at a time. Interrupting a call is
// safe through its context.
//
// Runtimes are reused by default. JavaScript state (globals the program
// writes, module-level variables) therefore persists between calls that
// happen to land on the same runtime. Set Config.NoReuse to create a fresh
// runtime for every call, or set MaxIdle to a lower number to keep fewer
// idle runtimes. A runtime is discarded after an interrupt, an internal
// error or a context cancellation, because timers and promises may still be
// pending in it.
//
// # Security
//
// moejs runs pure Go and provides memory limits, dynamic-code limits,
// interrupts and frozen builtins. jolt passes those settings through
// Config. A sandbox is only as strong as its features: HostFS confines every
// path to its Root, and HostNetwork with no Client has no network. Both
// accept Allow hooks. Do not run untrusted code with NoFS/NoNetwork replaced
// by an unrestricted feature.
package jolt

import (
	"fmt"
	"io"
	"io/fs"
	"runtime"
	"strings"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
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
	// base name of SourceFile, or "jolt.js".
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
	// installed by default, unless jolt is built with the "nowasm" tag (or
	// for GOARCH=wasm) or this option is disabled. The WASI host module is
	// loaded and its per-guest environment is taken from WebAssembly.WASI.
	// See WebAssemblySupported.
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
	// Normally jolt builds one from the resolver and the features.
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

// WebAssemblyOptions configures the WebAssembly JavaScript API.
//
// The API is compiled into the binary by default. Build with the "nowasm" tag
// to leave it out:
//
//	go build -tags nowasm ./...
//
// The opt-out tag cannot be named "wasm": Go reserves that name for
// GOARCH=wasm, and passing -tags wasm on another architecture makes the
// standard library select its wasm files and fail to compile. The API is also
// left out automatically when building for GOARCH=wasm.
//
// Without the API the WebAssembly global is not installed.
// WebAssemblySupported reports whether it is in the binary.
type WebAssemblyOptions struct {
	// Disable leaves the WebAssembly global uninstalled even in a build that
	// includes it. It is ignored without the API.
	Disable bool
	// MemoryLimitPages caps each WebAssembly linear memory in 64 KiB pages.
	// Zero means 65536, the browser limit (4 GiB).
	MemoryLimitPages uint32
	// WASI configures the wasi_snapshot_preview1 host module and the
	// environment attached to each guest that imports it.
	WASI WebAssemblyWASIOptions
}

// WebAssemblyWASIOptions configures WASI support. When the WebAssembly API is
// compiled in, the wasi_snapshot_preview1 host module is always instantiated
// so WASI-targeting modules load without the caller supplying WASI imports;
// set Disable to turn that off.
//
// The environment fields below are attached to every instantiated module that
// imports wasi_snapshot_preview1. Defaults are deliberately sandboxed: no
// arguments, no environment, no host file system, no stdio, and wazero's
// deterministic random_get source.
type WebAssemblyWASIOptions struct {
	// Disable leaves the WASI host module unloaded. A module that imports
	// wasi_snapshot_preview1 then needs the caller to supply those imports.
	Disable bool
	// Args is the argument vector reported by args_get.
	Args []string
	// Env is the environment reported by environ_get.
	Env map[string]string
	// FS is the guest file system; nil means no host file system.
	FS fs.FS
	// Stdin, Stdout and Stderr are the guest's standard streams.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Rand seeds random_get; nil uses wazero's deterministic default.
	Rand io.Reader
	// RunStart runs the "_start" export right after instantiation. The
	// browser default is to leave it to the caller.
	RunStart bool
}

// configured reports whether the caller asked for any WASI behavior. Disable
// on its own is not counted: without the API WASI is absent anyway.
func (o WebAssemblyWASIOptions) configured() bool {
	return len(o.Args) > 0 || len(o.Env) > 0 || o.FS != nil ||
		o.Stdin != nil || o.Stdout != nil || o.Stderr != nil ||
		o.Rand != nil || o.RunStart
}

// withDefaults returns a copy of c with zero values replaced by their
// defaults and the selected features in install order.
func (c Config) withDefaults() (Config, error) {
	if c.Source != "" && c.SourceFile != "" {
		return Config{}, fmt.Errorf("jolt: Config.Source and Config.SourceFile are both set")
	}
	if c.Source == "" && c.SourceFile == "" {
		return Config{}, ErrNoSource
	}
	if c.Mode < ModeAuto || c.Mode > ModeScript {
		return Config{}, fmt.Errorf("jolt: invalid Config.Mode %d", c.Mode)
	}
	if c.MemoryLimit < 0 {
		return Config{}, fmt.Errorf("jolt: Config.MemoryLimit is negative")
	}
	if c.PoolSize < 0 {
		return Config{}, fmt.Errorf("jolt: Config.PoolSize is negative")
	}
	if c.MaxIdle < 0 {
		return Config{}, fmt.Errorf("jolt: Config.MaxIdle is negative")
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
	if c.WebAssembly.MemoryLimitPages > 65536 {
		return Config{}, fmt.Errorf("jolt: WebAssembly.MemoryLimitPages %d exceeds the 65536-page (4 GiB) limit",
			c.WebAssembly.MemoryLimitPages)
	}
	if c.Name == "" {
		if c.SourceFile != "" {
			c.Name = baseName(c.SourceFile)
		} else {
			c.Name = "jolt.js"
		}
	}
	c.Modules = cloneStringMap(c.Modules)
	c.Globals = cloneAnyMap(c.Globals)
	c.Functions = cloneAnyMap(c.Functions)
	c.Features = cloneSlice(c.Features)
	c.WebAssembly.WASI.Args = cloneSlice(c.WebAssembly.WASI.Args)
	c.WebAssembly.WASI.Env = cloneStringMap(c.WebAssembly.WASI.Env)
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

// install adds the console global. The methods follow Node's console in
// spirit: log/info/debug write a line, warn/error also write a line (to the
// same writer), dir writes a JSON-ish representation, and assert writes an
// assertion failure.
func (c *consoleWriter) install(env *Env) error {
	line := func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		c.write([]byte(formatConsoleArgs(env.Raw().Realm(), args) + "\n"))
		return moejs.Undefined(), nil
	}
	dir := func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		if len(args) == 0 {
			c.write([]byte("undefined\n"))
			return moejs.Undefined(), nil
		}
		c.write([]byte(formatConsoleValue(r, args[0]) + "\n"))
		return moejs.Undefined(), nil
	}
	assert := func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		if len(args) > 0 && engine.ToBoolean(args[0]) {
			return moejs.Undefined(), nil
		}
		rest := args
		if len(rest) > 0 {
			rest = rest[1:]
		}
		text := "Assertion failed"
		if len(rest) > 0 {
			text += ": " + formatConsoleArgs(r, rest)
		}
		c.write([]byte(text + "\n"))
		return moejs.Undefined(), nil
	}
	ns := map[string]any{
		"log":    moejs.NativeFunc(line),
		"info":   moejs.NativeFunc(line),
		"debug":  moejs.NativeFunc(line),
		"warn":   moejs.NativeFunc(line),
		"error":  moejs.NativeFunc(line),
		"trace":  moejs.NativeFunc(line),
		"dir":    moejs.NativeFunc(dir),
		"assert": moejs.NativeFunc(assert),
	}
	return env.SetNamespace("console", ns)
}

func formatConsoleArgs(r *moejs.Realm, args []moejs.Value) string {
	var b strings.Builder
	for i, arg := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(formatConsoleValue(r, arg))
	}
	return b.String()
}

func formatConsoleValue(r *moejs.Realm, v moejs.Value) string {
	if v.IsString() {
		return v.String()
	}
	if v.IsObject() {
		if data, ok, err := r.AppendJSON(nil, v); err == nil && ok {
			return string(data)
		}
		return r.DisplayString(v)
	}
	return v.String()
}
