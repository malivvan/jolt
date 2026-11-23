# jolt

[![CI](https://github.com/malivvan/jolt/actions/workflows/ci.yml/badge.svg)](https://github.com/malivvan/jolt/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/malivvan/jolt.svg)](https://pkg.go.dev/github.com/malivvan/jolt)
[![Go Report Card](https://goreportcard.com/badge/github.com/malivvan/jolt)](https://goreportcard.com/report/github.com/malivvan/jolt)
[![Go Version](https://img.shields.io/github/go-mod/go-version/malivvan/jolt)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A developer-focused JavaScript executor for Go, built on
[moejs](https://github.com/MoeForge/moejs).

jolt turns moejs's low-level engine API into a small, ergonomic library:

- **One `Config` for everything** — source, engine limits, event loop, sandbox,
  imports, pool size and hooks.
- **Expose Go the easy way** — `SetFunc`, `SetNamespace`, `Set`, and
  `Config.Globals` accept ordinary Go funcs, maps and structs; no
  `moejs.NativeFunc` plumbing required.
- **Reach JavaScript the easy way** — `Call`, `CallInto`, `Get`, `Func`, plus
  `Value` and `Function` handles when the Go side drives several calls.
- **Compose the runtime** — choose the file system (`HostFS`, `NoFS`, your
  own), the network (`HostNetwork`, `NoNetwork`, your own), and any other
  `Feature`.
- **Run it concurrently** — `Executor` compiles the program once and serves
  calls from a bounded pool of runtimes.

```go
package main

import (
	"context"
	"fmt"

	"github.com/malivvan/jolt"
)

func main() {
	exec, err := jolt.New(jolt.Config{
		Name: "greet.js",
		Source: `
export function greet(name) {
  return utils.prefix + " " + name.trim();
}`,
		Globals: map[string]any{
			"utils": map[string]any{"prefix": "hello"},
		},
	})
	if err != nil {
		panic(err)
	}
	defer exec.Close()

	out, err := exec.Call(context.Background(), "greet", "  world  ")
	if err != nil {
		panic(err)
	}
	fmt.Println(out) // hello world
}
```

> The module path is `github.com/malivvan/jolt` and the package name is `jolt`.
> Adjust the module line in `go.mod` and the import paths if you fork it.

## Exposing Go to JavaScript

### Functions

`SetFunc` and `Config.Functions` register a Go function as a global host
function. The wrapper understands any function whose parameters and results
are JSON-shaped Go values:

```go
rt.SetFunc("sum", func(a, b int) int { return a + b })

rt.SetFunc("find", func(ctx context.Context, q Query) ([]Result, error) {
	return db.Find(ctx, q) // ctx ends when the JavaScript call is interrupted
})

rt.SetFunc("logf", func(format string, args ...any) {
	fmt.Printf(format, args...)
})
```

The supported signatures are:

```go
func(...)                 // no result
func(...) T               // T is converted to JavaScript
func(...) (T, error)      // error throws to JavaScript
func(...) error           // a non-nil error throws
```

A parameter may be:

| Go type | Receives |
| --- | --- |
| `context.Context` | the context of the running call (first parameter or any position) |
| `*jolt.Runtime` | the runtime, for callbacks during the call |
| `*moejs.Realm` | the engine realm |
| `moejs.Value` | the raw JavaScript value |
| `*jolt.Value` | a jolt value handle |
| `*moejs.Object` | the raw object value |
| a Go `func` type | the JavaScript function, callable from Go |
| any other Go type | decoded from the JavaScript value, with structs and maps using their `json` tags |

A `func` parameter receives the JavaScript function as a callable Go value, so
a host function can take a callback:

```go
rt.SetFunc("apply", func(fn func(int) int, n int) int { return fn(n) })
// apply(x => x + 1, 41) === 42
```

The callback's result type follows the same `(T)`, `(T, error)` and `error`
shapes as a host function; a throw inside the callback comes back as a Go
error. A variadic `func` parameter type is rejected.

Variadic parameters work. A Go panic is recovered and thrown as a JavaScript
`Error` (and exposed to Go as `*jolt.PanicError`). Named functions from
different runtimes are rejected, as moejs requires.

### Values and namespaces

`SetNamespace` recursively converts a map, including nested maps, slices,
funcs and `*Value` handles:

```go
rt.SetNamespace("host", map[string]any{
	"version": "1.2.3",
	"flags":   []string{"a", "b"},
	"add":     func(a, b int) int { return a + b },
})
```

`Set`/`SetPath` install one value and create nested objects as needed:

```go
rt.Set("host.timeout", 30*time.Second)
rt.SetPath("host.limits.maxBytes", 1<<20)
```

`Config.Globals` and `Config.Functions` do the same for every runtime at
creation, before the program is loaded.

## Accessing JavaScript from Go

### Calls

```go
// A path is resolved in module exports first, then in globals.
out, err := exec.Call(ctx, "buildRequest", task)

// Decode straight into a Go value.
var req Request
err = exec.CallInto(ctx, "buildRequest", &req, task)
```

Unless `Config.DisableAwaitPromises` is set, a returned promise is awaited on
the runtime's event loop. A rejection becomes a `*jolt.JSError`; a
synchronous `throw` remains a `*jolt.Exception` (an alias for
`moejs.Exception`).

A call runs the event loop until no referenced timer, immediate, task or held
promise remains. Pass a context (or set `Config.DefaultTimeout`) to bound a
program that never goes idle.

### Values and functions

For several calls against one JavaScript state, lease a runtime:

```go
err := exec.Run(ctx, func(rt *jolt.Runtime) error {
	// Module export or global.
	config, err := rt.Get("config")
	if err != nil {
		return err
	}
	retries, err := config.Get("retries")
	if err != nil {
		return err
	}
	n, err := retries.Int()

	// Resolve a function once and call it many times.
	add, err := rt.Func("makeAdder")
	if err != nil {
		return err
	}
	add5, err := add.Call(ctx, 5)
	if err != nil {
		return err
	}
	sum, err := add5.Call(ctx, 37) // 42
	return err
})
```

`Value` offers `ToGo`, `Into`, `Text`, `Bool`, `Int`, `Float`, `Type`,
`IsNull`, `IsUndefined`, `IsFunction`, `IsArray`, `IsPromise`, `Get`, `Set`,
`Call`, `CallMethod` and `Await`. `Function` wraps a resolved callable.

## Config

`Config` is intentionally broad. The zero value is usable as long as `Source`
or `SourceFile` is set.

| Group | Fields |
| --- | --- |
| Source | `Name`, `Source`, `SourceFile`, `TypeScript`, `Mode` (`ModeAuto`, `ModeModule`, `ModeScript`) |
| Engine | `TimeZone`, `Locale`, `MemoryLimit`, `MutableIntrinsics`, `MaxDynamicSource`, `DisableDynamicCode` |
| Compilation / execution | `CompileTimeout`, `DefaultTimeout`, `DisableAwaitPromises`, `Decode` (`MaxBytes`, `MaxNodes`) |
| Event loop | `OnLoopError`, `ResetMemoryPerTask`, `OnIdle`, `OnWake`, `OnUnhandledRejection` |
| Features | `FS`, `Network`, `Features` |
| Modules | `Modules`, `Loader`, `ModuleFS`, `ModuleRoot`, `Resolve`, `Importer` |
| Exposure | `Globals`, `Functions`, `Console`, `DisableConsole` |
| WebAssembly | `WebAssembly` (`Disable`, `MemoryLimitPages`, `WASI`: `Disable`, `Args`, `Env`, `FS`, `Stdin`, `Stdout`, `Stderr`, `Rand`, `RunStart`); on by default, opt out with `-tags nowasm` |
| Pool | `PoolSize`, `MaxIdle`, `NoReuse` |
| Hooks | `OnRuntimeReady`, `OnRuntimeClose` |

Defaults:

- ES module; `.ts`/`.mts`/`.cts`/`.tsx` names are TypeScript automatically.
- `PoolSize` = `GOMAXPROCS`; `MaxIdle` = `PoolSize`.
- A returned promise is awaited.
- No file system (`NoFS`), no network/Web APIs (`NoNetwork`).
- Timers and the event loop are always installed.
- A `console` global is installed; its output goes to `Config.Console` (nil
  discards it). Set `DisableConsole` to remove it.
- Builtins are shared, frozen and read-only (`MutableIntrinsics` opts out).
- `MaxDynamicSource` and `MemoryLimit` follow moejs: `0` means the engine
  default or no limit.

## Composable runtimes

A runtime is assembled from `Feature`s:

```go
type Feature interface {
	Install(env *Env) (cleanup func() error, err error)
}
```

`Env` exposes the runtime, its event loop and the same helpers as `Runtime`.
A feature can install globals, modules or policies. `Config.FS` and
`Config.Network` are just features:

```go
exec, err := jolt.New(jolt.Config{
	Source: source,

	// node:fs, node:fs/promises and Buffer over one sandbox directory.
	FS: &jolt.HostFS{
		Root:  "/srv/workspaces/123",
		Mount: "/workspace",
		Write: false,
		Allow: func(op, path string) error {
			if op == "write" {
				return fs.ErrPermission
			}
			return nil
		},
	},

	// fetch and the Web APIs through one HTTP client.
	Network: &jolt.HostNetwork{
		Client: http.DefaultClient,
		Allow:  jolt.AllowHosts("api.example.com", "*.example.org"),
	},

	// Anything else.
	Features: []jolt.Feature{
		jolt.FeatureFunc(func(env *jolt.Env) (func() error, error) {
			return nil, env.SetFunc("now", func() int64 { return time.Now().Unix() })
		}),
	},
})
```

- `NoFS{}` and `NoNetwork{}` install nothing.
- `&HostNetwork{Client: nil}` installs `fetch`, `URL`, `Response`, Streams,
  etc. but every fetch rejects: useful for SDKs that reference the globals but
  must not reach the network.
- A feature may implement `ResolverProvider` and `ImporterProvider` to serve
  its own `import` specifiers, which is how `HostFS` provides
  `node:fs`.
- Scoping: the first feature that knows a specifier wins.

You can also replace a module with ordinary configuration:

```go
Config{
	Modules: map[string]string{
		"node:fs":   myFSModuleSource,
		"my:config": `export const region = "eu-west-1";`,
	},
	Globals: map[string]any{
		"__fs": map[string]any{"read": myRead, "write": myWrite},
	},
}
```

## WebAssembly

jolt installs the browser `WebAssembly` JavaScript API, backed by
[wazero](https://github.com/tetratelabs/wazero), by default. Build with the
`nowasm` tag to leave it out:

```sh
go build -tags nowasm ./...
go test -tags nowasm ./...
```

> Why not tag it `wasm`? Go reserves `wasm` for `GOARCH=wasm`. Passing
> `-tags wasm` while targeting amd64/arm64 makes the standard library select its
> wasm files and fail to compile. The API is also automatically absent when
> cross-compiling for `GOARCH=wasm`.
> `jolt.WebAssemblySupported` reports whether the feature is in the binary.

```go
exec, err := jolt.New(jolt.Config{
	Name: "wasm.js",
	Source: `export async function add(bytes) {
		const { instance } = await WebAssembly.instantiate(bytes, {});
		return instance.exports.add(2, 3);
	}`,
})
out, err := exec.Call(ctx, "add", wasmBytes) // []byte becomes an ArrayBuffer
```

Supported:

- `WebAssembly.Module`, `Instance`, `Memory`, `Global`, `Table`
- `compile`, `instantiate`, `validate`, `compileStreaming`,
  `instantiateStreaming`
- `Module.exports`, `Module.imports`, `Module.customSections`
- `CompileError`, `LinkError`, `RuntimeError`
- function, memory and global imports; exported functions, memories and globals
- i32, i64 (BigInt), f32 and f64 values, and multi-value results
- live `memory.buffer` views and `memory.grow`

The wazero manager is configured like a browser: Core 2.0 features, a 4 GiB
memory ceiling, and no automatic `_start` (set `WASI.RunStart` to run it). A
caller's context still cancels a running wasm function, and
`Config.WebAssembly.MemoryLimitPages` can lower the 65536-page ceiling.
`Config.WebAssembly.Disable` leaves the global out even in a tagged build.

### WASI

The `wasi_snapshot_preview1` host module is loaded automatically, so a module
that imports WASI instantiates without the JavaScript caller supplying those
imports. A user-supplied `wasi_snapshot_preview1` object in `importObject` is
ignored; the host-managed namespace always wins.

Each guest that imports WASI gets its environment from the Go config:

```go
exec, err := jolt.New(jolt.Config{
	WebAssembly: jolt.WebAssemblyOptions{
		WASI: jolt.WebAssemblyWASIOptions{
			Args:   []string{"app", "--flag"},
			Env:    map[string]string{"MODE": "prod"},
			FS:     os.DirFS("/srv/app"),
			Stdout: os.Stdout,
			Stderr: os.Stderr,
			Rand:   rand.Reader,
			RunStart: true, // run _start at instantiation
		},
	},
	Source: `export async function run(bytes) {
		await WebAssembly.instantiate(bytes, {});
		return "ok";
	}`,
})
```

Defaults are sandboxed: no arguments, no environment, no host file system, no
stdio, no clocks, and wazero's deterministic `random_get` source. Set
`WebAssembly.WASI.Disable` to leave the host module unloaded. `RunStart` is off
by default, keeping browser semantics; a command module can instead call
`instance.exports._start()` itself. `proc_exit(0)` is treated as success and a
non-zero exit becomes a `RuntimeError`. `wasi_unstable` is not loaded.

Known limits of wazero's public API: `WebAssembly.Table` is present for API
shape, but `get`/`set`/`grow` throw and imported tables are rejected with a
`LinkError`. Shared memories, `v128`, and reference-type function signatures are
not supported. An exported memory with no maximum is capped by wazero at 65536
pages, so importing it into a module that requires a smaller maximum is a
`LinkError`, as it is in a browser.

## Modules and imports

Static imports are linked once, when the `Executor` is created. Dynamic
`import()` and `import.meta` go through a shared `moejs.Importer`.

Resolution order for a specifier:

1. `Config.Modules` exact match (then relative candidates).
2. `Config.Loader` (`ModuleLoader`).
3. `Config.ModuleFS` + `Config.ModuleRoot`, an `io/fs` file system (for
   `embed.FS`, for example).
4. The host directory of `Config.SourceFile`.

A `SourceSet` is a `ModuleLoader` over a map. `NewFSLoader(fsys, root)` loads
from any `io/fs`. Host module loading is confined to the entry file's
directory; the file system a *script* sees is still controlled by `FS`.

## Concurrency and lifetime

`Executor` is safe for concurrent use. Each `Call`, `CallInto` or `Run` leases
one runtime; `PoolSize` bounds how many run together.

`Runtime` is not safe for concurrent use: moejs requires one goroutine at a
time. Cancelling the context passed to a call is safe from another goroutine:
it interrupts the loop and the call returns `*jolt.InterruptedError`.

Runtime state persists between calls that use the same pooled runtime. Set
`NoReuse` for a fresh runtime per call, or use `NewRuntime` when the program is
a long-lived state machine. A runtime is discarded after an interrupt,
timeout, memory-limit hit or internal error, because timers and promises may
still be pending in it.

## Security

jolt passes moejs's defenses through `Config`:

- `MemoryLimit` caps retained and allocated bytes; a hit is
  `jolt.IsMemoryLimit(err)`.
- `DisableDynamicCode` turns off `eval` and `new Function`;
  `MaxDynamicSource` caps their source length.
- `MutableIntrinsics` is off by default, so builtins are frozen.
- `HostFS` confines every path to `Root` through `os.Root` and is read-only
  unless `Write` is set; `Allow` sees each access.
- `HostNetwork` with no `Client` has no network; `Allow`/`AllowHosts` approve
  each request, redirects included.
- `NoFS`/`NoNetwork` are the safe defaults.
- WebAssembly is on by default (`-tags nowasm` removes it); it runs in wazero
  with WASI loaded but sandboxed by default (no args, environment, file system,
  stdio or clocks, and deterministic randomness) and honors
  `Config.WebAssembly.MemoryLimitPages`. `Config.WebAssembly.WASI` exposes host
  state deliberately; treat a non-empty `FS` or real `Stdout` as capabilities.

Do not run untrusted code with `Config.Importer`, `Config.Resolve` or a custom
module loader that exposes host files or modules you did not intend to expose.

## Errors

jolt wraps engine errors in `*jolt.Error` (which implements `Unwrap`), so
`errors.Is` and `errors.As` keep working. Common helpers:

```go
jolt.IsException(err)    // a JavaScript throw; AsException gets it
jolt.IsInterrupted(err)  // context cancellation or an explicit interrupt
jolt.IsTimeout(err)      // context deadline
jolt.IsCanceled(err)     // context cancellation
jolt.IsMemoryLimit(err)  // Config.MemoryLimit hit
jolt.JSStack(err)        // JavaScript stack trace, when there is one
```

`JSError` is a rejected promise's reason converted to Go. `PanicError` is a Go
panic inside a host function.

## Examples and tests

Runnable examples live in [`demos/`](demos). The test suite covers
exposure, access, modules, TypeScript, async/timers, rejection, timeouts,
memory limits, `HostFS`, `HostNetwork`, pooling and custom features:

```sh
go test ./...
go test -race ./...
```
