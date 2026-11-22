// Package canopy is a developer-focused JavaScript executor built on the
// moejs engine.
//
// Canopy wraps moejs with the pieces a Go program usually ends up writing
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
//	exec, err := canopy.New(canopy.Config{
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
// *Value receives a canopy handle. A function may be variadic and may return
// (T), (T, error) or error. A Go panic inside a registered function becomes a
// *PanicError thrown back to JavaScript; JavaScript may catch it as an Error.
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
//	func (m metrics) Install(env *canopy.Env) (func() error, error) {
//		return nil, env.SetFunc("recordMetric", m.record)
//	}
//
//	exec, err := canopy.New(canopy.Config{
//		Source:  source,
//		Network: &canopy.HostNetwork{Client: client, Allow: canopy.AllowHosts("api.example.com")},
//		Features: []canopy.Feature{metrics{sink}},
//	})
//
// # WebAssembly
//
// Building with the "canopy_wasm" tag installs the browser WebAssembly
// JavaScript API on top of wazero:
//
//	go build -tags canopy_wasm ./...
//
// The tag is not named "wasm" because Go reserves that name for GOARCH=wasm;
// enabling it on another architecture makes the standard library select its
// wasm files and fail to compile. WebAssemblySupported reports whether the
// binary has the feature.
//
// The manager is configured like a browser: WebAssembly Core 2.0 features, a
// 4 GiB memory ceiling, no WASI, no host file system, no environment or stdio,
// and no automatic "_start". Config.WebAssembly.Disable leaves the global out;
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
// interrupts and frozen builtins. Canopy passes those settings through
// Config. A sandbox is only as strong as its features: HostFS confines every
// path to its Root, and HostNetwork with no Client has no network. Both
// accept Allow hooks. Do not run untrusted code with NoFS/NoNetwork replaced
// by an unrestricted feature.
package js
