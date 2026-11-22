# WAZERO.md — wazero module inspection & customized loading

This document records the findings of researching how canopy's WebAssembly
backend (`wasm.go`, `wasm_binary.go`, `wasm_values.go`, gated by the
`canopy_wasm` build tag) can *inspect* wazero modules and load them in a
customized way for compatibility. The concrete goal used throughout is:
**make WASI "always loaded"**, so that a wasm module compiled against
`wasi_snapshot_preview1` instantiates and runs without the JavaScript caller
having to supply WASI imports by hand.

Everything below was verified against `github.com/tetratelabs/wazero v1.12.0`
(the version pinned in `go.mod`).

---

## 1. How canopy loads wasm today

One `wasmState` owns **one** `wazero.Runtime` per moejs runtime
(`installWebAssembly` in `wasm.go`). The runtime is created with Core 2.0
features, a 4 GiB memory ceiling, `WithCloseOnContextDone(true)`, and
`WithCustomSections(true)`.

The load path is:

1. **Compile** — `wasmState.compile` calls `wr.CompileModule(ctx, binary)` and
   also runs canopy's own `parseWasmBinary(binary)` to produce a
   `wasmBinaryInfo` (imports/exports/tables/memories/globals, `hasStart`).
2. **Instantiate** — `wasmState.instantiate` calls `prepareImports`, which
   walks `m.info.imports` and requires *every* import to be present in the
   JavaScript `importObject`. It builds synthetic "namespace" modules (via
   `wasmModuleBuilder`) that re-export the JS functions/memories/globals under
   the names the guest imports. The result is a `map[string]api.Module` keyed
   by import module name, supplied to wazero through
   `experimental.WithImportResolver`.
3. The guest is instantiated with
   `wazero.NewModuleConfig().WithName(name).WithStartFunctions()` — note the
   empty `WithStartFunctions()` **disables** the default `_start` run
   (`NewModuleConfig()` defaults to `[]string{"_start"}`; passing no args
   replaces it with an empty slice). This matches browser semantics: a browser
   never runs `_start`.

Two consequences drive the whole design:

- `prepareImports` **throws a `LinkError` for any import whose module is not in
  `importObject`**. So today a WASI module (`(import "wasi_snapshot_preview1"
  "fd_write" ...)`) fails to instantiate unless the caller passes a
  `wasi_snapshot_preview1` object — and that is not how real WASI is wired.
- The guest's `ModuleConfig` carries **no args/env/FS/stdio**, so even if the
  imports resolved, the WASI syscalls would have no environment to work on.

---

## 2. wazero's module-inspection surface

There are two layers available, and canopy already uses both.

### 2.1 Public API — `wazero.CompiledModule`

Defined in wazero `config.go` (`type CompiledModule interface`):

| Method | Returns | Notes |
| --- | --- | --- |
| `Name() string` | module name | From the binary's name section; empty if absent |
| `ImportedFunctions()` | `[]api.FunctionDefinition` | In binary import order; no unique constraint |
| `ExportedFunctions()` | `map[string]api.FunctionDefinition` | Keyed by export name |
| `ImportedMemories()` | `[]api.MemoryDefinition` | At most one memory (Core 2.0) |
| `ExportedMemories()` | `map[string]api.MemoryDefinition` | Keyed by export name |
| `CustomSections()` | `[]api.CustomSection` | `Name()` / `Data()` |
| `Close(ctx)` | `error` | Release the compiled code |

`api.FunctionDefinition` embeds `api.ExportDefinition`:

- `Import() (moduleName, name string, isImport bool)` — **this is how you get
  the module name and import name of an imported function**. Verified in
  wazero `internal/wasm/function_definition.go` (the `importDesc` is filled
  from the import section).
- `Name() string`, `DebugName() string` (e.g. `"env.abort"`), `Index() uint32`
- `ParamTypes() []api.ValueType`, `ParamNames() []string`,
  `ResultTypes() []api.ValueType`, `ResultNames() []string`
- `GoFunction() interface{}` — non-nil when the function is a Go host function
  (i.e. it came from a `HostModuleBuilder`), not a wasm binary.
- `ExportNames() []string`

`api.MemoryDefinition` embeds `ExportDefinition` and adds `Min() uint32` and
`Max() (uint32, bool)` (bool = "max is encoded").

### 2.2 What the public API does *not* expose

- **No `ImportedGlobals` / `ImportedTables`** and no global/table definition
  types at all. Function and memory definitions are the only import/export
  definitions wazero exposes publicly.
- `ImportedFunctions()` is **flat**, not grouped by module. Regroup by calling
  `.Import()` on each definition.
- No access to `ImportPerModule` (the internal grouping wazero itself uses in
  `internal/wasm/store.go` `resolveImports`); that map is internal.

That is exactly why canopy keeps `parseWasmBinary` (`wasm_binary.go`): it reads
the type/import/table/memory/global/export/start sections and records module
names and kinds for *every* import kind, which the public API cannot fully
provide. For the WASI use case, `wasmBinaryInfo.imports[].module` is the most
complete source of truth.

### 2.3 Canonical wazero example of import inspection

wazero's own CLI (`cmd/wazero/wazero.go`) does exactly the detection we want:

```go
func detectImports(imports []api.FunctionDefinition) importMode {
	for _, f := range imports {
		moduleName, _, _ := f.Import()
		switch moduleName {
		case wasi_snapshot_preview1.ModuleName: // "wasi_snapshot_preview1"
			return modeWasi
		case "wasi_unstable":
			return modeWasiUnstable
		}
	}
	return modeDefault
}
```

Note it inspects only imported *functions*. canopy's `parseWasmBinary` sees the
module name of every import kind, so it can be more thorough (a module that
imports only a WASI memory/global, no functions, would still be detected).

---

## 3. How wazero resolves imports

`internal/wasm/store.go` `resolveImports` iterates the internal
`ImportPerModule` map (one entry per import module name). For each module name:

1. If an `experimental.ImportResolver` is present in the context, call it with
   **the module name only** (not the import name). A non-nil return is used.
2. Otherwise fall back to the store lookup by module name:
   `m.s.module(moduleName)`.

Then each import within that module is resolved against the chosen module's
exports, with signature checking.

Two facts matter:

- **The resolver is module-name granular.** It cannot satisfy a *subset* of a
  module's imports — if a resolver returns a module for `"wasi_snapshot_preview1"`,
  it must export every function that module is asked for (or the store's
  regular lookup path handles the rest). In practice, for WASI you return the
  whole `wasi_snapshot_preview1` host module.
- **Module names are unique per runtime.** `Store.registerModule` returns
  `module[<name>] has already been instantiated` if a name is already in the
  store. So the WASI host module can be instantiated **at most once** per
  `wazero.Runtime`, and it must be done idempotently.

`experimental.WithImportResolver(ctx, func(name string) api.Module)` is the
mechanism canopy already uses for JS-supplied namespaces
(`wasm.go` `instantiate`).

---

## 4. The WASI host module

### 4.1 Import path and names

```go
import wasi_snapshot_preview1 "github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
```

This is a sub-package of the wazero module already in `go.mod`; **no new
dependency** is needed.

- `wasi_snapshot_preview1.ModuleName` == `"wasi_snapshot_preview1"`.
- The legacy alias `"wasi_unstable"` is not a named constant. It is built by
  exporting the same functions into a host module of that name:

```go
b := wr.NewHostModuleBuilder("wasi_unstable")
wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(b)
_, err := b.Instantiate(ctx)
```

### 4.2 Instantiation

```go
// Load the host module. No config is needed here — it is a stateless Go host
// module whose functions read the *calling* module's sys.Context:
wasi_snapshot_preview1.MustInstantiate(ctx, wr)          // panics on error
closer, err := wasi_snapshot_preview1.Instantiate(ctx, wr)
```

**Important:** the WASI environment (args/env/FS/stdio/rand) does **not** go on
the WASI host module's own instantiation. WASI functions are
`api.GoModuleFunc`s; their `mod api.Module` parameter is the **calling
module**, and they read `mod.(*wasm.ModuleInstance).Sys` — the *guest's*
`sys.Context` (verified in `imports/wasi_snapshot_preview1/fs.go`:
`mod.(*wasm.ModuleInstance).Sys.FS()`). So the environment must be attached to
the **guest** module's `ModuleConfig` at its instantiation (see §5 and §6.4):

```go
// The GUEST is instantiated with the WASI environment:
mod, err := wr.InstantiateModule(ctx, guestCompiled, wazero.NewModuleConfig().
    WithName("app").
    WithArgs("app", "arg1").
    WithEnv("KEY", "value").
    WithFS(os.DirFS("/some/dir")).
    WithStdout(os.Stdout).
    WithStderr(os.Stderr).
    WithRandSource(rand.Reader))
```

`InstantiateModule` registers the host module under the name from
`ModuleConfig.WithName`, falling back to the compiled module's name section
(`"wasi_snapshot_preview1"`); either way the store lookup for that name finds
it.

### 4.3 Configuration surface (`wazero.ModuleConfig`)

All WASI environment comes from `ModuleConfig` (`config.go`), **not** from the
wasi package itself:

- `WithArgs(...string)`, `WithEnv(key, value string)`
- `WithFS(fs fs.FS)`, `WithFSConfig(FSConfig)` (dir mounts, read-only mounts,
  `fs.FS`/`experimental/sys.FS` mounts)
- `WithStdin(io.Reader)`, `WithStdout(io.Writer)`, `WithStderr(io.Writer)`
- `WithRandSource(io.Reader)` — default is a **deterministic** source
- `WithWalltime`/`WithSysWalltime`, `WithNanotime`/`WithSysNanotime`,
  `WithNanosleep`/`WithSysNanosleep`, `WithOsyield` — **all default to
  unsupported/no-op**, deliberately, to preserve sandboxing. Without
  `WithSysWalltime`, WASI `clock_time_get(REALTIME)` returns `ENOSYS` and
  `random_get` without `WithRandSource` returns deterministic bytes.

### 4.4 `proc_exit` and `sys.ExitError`

WASI `proc_exit(code)` calls `Module.CloseWithExitCode`. Consequences:

- After `_start` runs and calls `proc_exit`, the **guest module instance is
  closed**. `proc_exit(0)` is treated as success by `InstantiateModule`
  (`*sys.ExitError` with `ExitCode()==0` is swallowed); a non-zero code is
  returned to the caller as a `*sys.ExitError`
  (`github.com/tetratelabs/wazero/sys`).
- Because canopy runs `_start` manually (see §6.4) rather than letting
  `InstantiateModule` run it, canopy must map `*sys.ExitError` to a
  `RuntimeError`/`LinkError` itself, and must be aware the module may be closed
  after `_start` returns (check `api.Module.IsClosed()`).

---

## 5. The two halves of "ensure WASI is always loaded"

Loading WASI is **not one step**. Verified from wazero internals
(`runtime.go` `InstantiateModule` → `config.toSysContext()` →
`internalsys.NewContext`): the args/env/stdin/stdout/stderr/FS/rand/clocks are
attached to the **guest module's own `sys.Context`**, created per
`InstantiateModule` call from the guest's `ModuleConfig`. The WASI host module
is a stateless Go host module whose functions read that `sys.Context` from the
calling module.

So two things must happen:

1. **Host module loaded** — instantiate `wasi_snapshot_preview1` (and
   optionally `wasi_unstable`) into the runtime so the guest's WASI imports
   resolve.
2. **Guest environment populated** — instantiate the guest with a
   `ModuleConfig` carrying `WithArgs/WithEnv/WithFS/WithStdout/…`. Otherwise
   the imports resolve but every syscall sees an empty/no-op environment
   (`args_get` → zero args, `fd_write` → `EBADF`, `clock_time_get` → `ENOSYS`).

---

## 6. Recommended implementation plan

### 6.1 Detection — classify each import's module

At instantiation time canopy already has `m.info.imports` with
`imp.module` for every import kind. Classify each import into one of:

- **host-managed** (e.g. `wasi_snapshot_preview1`, optionally `wasi_unstable`,
  and later `env`/`assemblyscript`/`emscripten`): skip it in `prepareImports`,
  let wazero's store resolve it.
- **JS-managed** (everything else): current behavior — require
  `importObject[module]` and build a synthetic namespace.

Add a small helper, e.g.:

```go
func (s *wasmState) isHostNamespace(module string) bool {
	switch module {
	case wasi_snapshot_preview1.ModuleName, "wasi_unstable":
		return true
	}
	return false
}
```

### 6.2 Skip host-managed imports in `prepareImports`

`prepareImports` (`wasm.go`) currently walks `m.info.imports` and, for every
entry, requires `importObject[imp.module]`. It also keeps an index `fi` into
`m.compiled.ImportedFunctions()`, advanced only for `wasmKindFunc` imports.

Both `m.info.imports` and `ImportedFunctions()` are in the same binary import
order (verified: `ImportedFunctions()` walks `ImportFunctionCount` entries of
the import section in order; `parseWasmImports` walks the same section in
order). So the change is mechanical:

- At the top of the loop, if `s.isHostNamespace(imp.module)`, `continue` —
  **but still advance `fi` when `imp.kind == wasmKindFunc`** so the function
  definitions stay aligned with the JS namespaces. (Or, more robustly, build
  the host-namespace filter over `ImportedFunctions()` and index by
  `fd.Index()`, matching on `Import()` names.)

Because host-managed imports are skipped, `resolved` will contain only
JS namespaces; wazero's store lookup handles the rest.

### 6.3 Load the host module once per runtime

In `wasmState.install` (or immediately after `NewRuntimeWithConfig`), eagerly
instantiate WASI into `s.wr`:

```go
// Minimum for "always loaded", safe defaults:
wasi_snapshot_preview1.MustInstantiate(ctx, s.wr)
```

The host module itself needs no `ModuleConfig` — its functions read the
*guest's* `sys.Context` (§4.2). The environment is attached to the guest in
§6.4. Keep the returned `api.Closer` so the runtime cleanup can close it, or
rely on `wr.Close` closing everything it created.

> Eager vs lazy: eager is the simplest match for "always loading" and avoids
> any detection-before-resolve race, but pays the cost of the host module on
> every runtime even when no WASI module is ever instantiated. A lazy variant
> loads it on first detection (§6.1) inside `instantiate`, guarded by a
> `sync.Once` on `wasmState`. Both are correct because the resolver/store path
> is consulted at instantiation time. Pick eager unless the per-runtime cost
> matters; the executor pools many runtimes.

### 6.4 Build the guest `ModuleConfig` with the WASI environment

Change `wasmState.instantiate` to build the guest config from the WASI options
instead of the bare `WithName(...).WithStartFunctions()`:

```go
mc := wazero.NewModuleConfig().WithName(name).WithStartFunctions() // browser: no _start
if s.wasiEnabled && m.info.hasWasiImports {
	mc = mc.
		WithArgs(s.wasiArgs...).
		WithEnv(k, v)...                 // iterate map
	if s.wasiFS != nil { mc = mc.WithFS(s.wasiFS) }
	if s.wasiStdout != nil { mc = mc.WithStdout(s.wasiStdout) }
	// ... stderr/stdin/rand/clocks as configured
}
mod, err := s.wr.InstantiateModule(ictx, m.compiled, mc)
```

Decide explicitly whether to run `_start`. The browser-faithful default is
**no** `_start`; the caller then runs `instance.exports._start()` explicitly.
For a WASI *command* that must run to completion, add an option to pass
`WithStartFunctions("_start")` (or call `_start` right after instantiation and
translate `sys.ExitError`). For a WASI *reactor*, `_initialize` is the entry
point and is likewise opt-in.

### 6.5 Map `proc_exit` correctly

When `_start`/`_initialize` is run, capture `*sys.ExitError` and either treat
`ExitCode()==0` as success or surface the code. Non-zero exit should become a
`RuntimeError` (or a dedicated error) via `wasmState.throwError`, matching the
existing `hasStart`-based class selection.

### 6.6 Option plumbing in `WebAssemblyOptions`

Add a WASI block to `WebAssemblyOptions` (`wasm_options.go`), e.g.:

```go
type WebAssemblyOptions struct {
	Disable bool
	MemoryLimitPages uint32
	WASI WebAssemblyWASIOptions
}

type WebAssemblyWASIOptions struct {
	Disable bool          // default false → WASI host module always loaded
	Args    []string
	Env     map[string]string
	FS      fs.FS         // nil = no host FS (WASI still works for pure compute)
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Rand    io.Reader
	RunStart bool          // run `_start` for command modules; false = browser semantics
}
```

`Config.WebAssembly` is documented in `doc.go` and `README.md`; update the
"no WASI" wording when this lands.

---

## 7. Edge cases & gotchas

- **`prepareImports` runs before wazero resolves.** Loading the WASI host
  module is *not* enough by itself — `prepareImports` would still throw a
  `LinkError` for the missing `wasi_snapshot_preview1` namespace. The import
  walk must skip host namespaces (§6.2).
- **Guest `sys.Context` comes from the guest's `ModuleConfig`.** Without
  `WithArgs/WithEnv/WithFS/WithStdout` on the guest config, resolved WASI
  imports still see an empty environment (§5).
- **Index alignment.** `m.info.imports` and `m.compiled.ImportedFunctions()`
  are both in binary import order; when skipping host-managed imports, keep
  `fi` advancing for function imports or the JS namespace function types will
  mismatch.
- **One instance per name.** `wasi_snapshot_preview1` can be instantiated once
  per `wazero.Runtime`; a second instantiation under the same name errors. Use
  a `sync.Once` if loading lazily.
- **`ImportResolver` is per-module-name.** You cannot supply half of WASI
  through the resolver and half through the store; return the whole module.
- **`proc_exit` closes the guest.** After `_start` runs, the `api.Module` may
  be `IsClosed()`; guard subsequent `ExportedFunction` calls.
- **Determinism / sandboxing.** Defaults are deliberately empty: deterministic
  `random_get`, no wall/mono clocks, no FS, no stdio. Exposing real clocks/FS
  is an explicit opt-in via options.
- **Shared host module, per-guest context.** One WASI host module serves many
  guests in a runtime; the fd table/args/env are per guest (via `sys.Context`),
  so guests do not share files unless they share an FS/`ModuleConfig`.
- **`WithCloseOnContextDone(true)`** (already set): cancelling a call closes
  the modules, so a long-running `_start` is interruptible.
- **Memory requirement.** WASI ABI requires the guest to export a memory named
  `"memory"`; that flows through the existing export/`wasmMemory` bridge
  unchanged.

---

## 8. Testing approach

- **Build gate:** everything stays behind `canopy_wasm`; run
  `go build -tags canopy_wasm ./...` and `go test -tags canopy_wasm ./...`.
- **A WASI test binary:** build a TinyGo `-target=wasi` or a
  wasi-sdk/C `main.c` into a `test.wasm` that prints via `fd_write` and calls
  `proc_exit(0)`. Keep it out of the repository if it is large; generate it
  under `$TMPDIR` or commit a tiny hand-assembled WASI wat.
- **Unit tests:**
  - `Module.imports()` on a WASI binary shows `wasi_snapshot_preview1`
    entries (parser already does this).
  - Instantiation with no `importObject` succeeds once the host namespace is
    auto-loaded (currently it throws `LinkError`).
  - `instance.exports._start()` runs and returns; non-zero `proc_exit` yields
    the expected error class.
  - `WebAssembly.instantiate(bytes, { wasi_snapshot_preview1: {...} })` still
    resolves user-supplied namespaces first (precedence test).
  - Config plumbing: `WithArgs`/`WithEnv`/`WithFS` observable via
    `args_get`/`environ_get`/`path_open`.

---

## 9. File map

| File | Role |
| --- | --- |
| `wasm.go` | `wasmState`, `compile`, `instantiate`, `prepareImports`, `buildNamespace`, import resolver wiring |
| `wasm_binary.go` | `parseWasmBinary` + `wasmBinaryInfo` (full import/export introspection), `wasmModuleBuilder` (synthetic modules) |
| `wasm_values.go` | memory/global/table bridging, value encoding, `buildExports` |
| `wasm_options.go` | `WebAssemblyOptions` — where the WASI option block goes |
| `wasm_off.go` | no-tag stub of `installWebAssembly` |
| `runtime.go` (wazero) | `InstantiateModule`, `NewModuleConfig`, `WithStartFunctions` default |
| `config.go` (wazero) | `CompiledModule` interface, `ModuleConfig` WASI config surface |
| `imports/wasi_snapshot_preview1` (wazero) | `ModuleName`, `Instantiate`, `MustInstantiate`, `NewBuilder`, `NewFunctionExporter` |
| `internal/wasm/store.go` (wazero) | import resolution order, duplicate-name error |
| `cmd/wazero/wazero.go` (wazero) | `detectImports` reference implementation |
