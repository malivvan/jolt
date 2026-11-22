# PLAN.md — WASI auto-loading for the WebAssembly backend

Converted from `WAZERO.md` (research findings) into an execution plan. All code
lives behind the `canopy_wasm` build tag. Target: wazero `v1.12.0` (pinned).

## Objective

Make the `WebAssembly` backend **always load the WASI host module**, so a wasm
module that imports `wasi_snapshot_preview1` instantiates and runs **without the
JavaScript caller supplying WASI imports**. The environment (args/env/FS/stdio)
is attached to the guest, per the two-halves model in WAZERO.md §5.

## Design decisions (resolving WAZERO.md's open choices)

1. **WASI is on by default.** `WebAssemblyOptions.WASI.Disable` opts out.
2. **Host-managed namespaces win.** If a module imports `wasi_snapshot_preview1`,
   it is always satisfied by the real WASI host module; a user-supplied
   `wasi_snapshot_preview1` object in `importObject` is ignored. (Simplest
   behavior matching "WASI always loaded"; alternative precedence is a
   follow-up.)
3. **Browser semantics retained:** `_start` is not auto-run. WASI command
   modules can call `instance.exports._start()` manually. An optional
   `RunStart` flag (Step 6) runs it at instantiation.
4. **Scope:** `wasi_snapshot_preview1` only. `wasi_unstable` and
   `env`/`assemblyscript`/`emscripten` namespaces are out of scope (noted in
   Step 6 as optional follow-ups).

## Verification commands (run after each phase)

```sh
go build ./...                     # untagged must still compile (wasm_off.go)
go build -tags canopy_wasm ./...
go vet -tags canopy_wasm ./...
go test ./...                      # untagged
go test -tags canopy_wasm ./...
```

---

## Phase A — core implementation

### Step 1. Extend `WebAssemblyOptions` (`wasm_options.go`)

Add imports `io` and `io/fs`, and a nested WASI options struct.

```go
import (
	"io"
	"io/fs"
)

// WebAssemblyOptions configures the WebAssembly JavaScript API.
// ... (keep existing doc)
type WebAssemblyOptions struct {
	Disable bool
	MemoryLimitPages uint32
	WASI WebAssemblyWASIOptions
}

// WebAssemblyWASIOptions configures WASI support. When canopy is built with
// the canopy_wasm tag, the wasi_snapshot_preview1 host module is always
// instantiated so WASI-targeting modules load without the caller supplying
// WASI imports; set Disable to turn that off. The environment fields below
// are attached to each instantiated WASI module's sys.Context (see WAZERO.md).
type WebAssemblyWASIOptions struct {
	Disable bool          // leave the WASI host module unloaded
	Args    []string
	Env     map[string]string
	FS      fs.FS         // nil = no host FS (WASI still works for pure compute)
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Rand    io.Reader     // nil = wazero's deterministic default
	RunStart bool         // run `_start` at instantiation (Step 6)
}
```

### Step 2. Thread WASI state into `wasmState` and load the host module (`wasm.go`)

2a. Add the import (same module already in `go.mod`, no new dependency):

```go
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
```

2b. Extend `wasmState`:

```go
type wasmState struct {
	rt               *Runtime
	env              *Env
	wr               wazero.Runtime
	realm            *engine.Realm
	count            uint64
	memoryLimitPages uint32
	wasi             WebAssemblyWASIOptions
	wasiEnabled      bool
	// ...existing class fields...
}
```

2c. In `installWebAssembly`, build `s` with the new fields and eagerly
instantiate WASI (WAZERO.md §6.3). Replace the `s := &wasmState{...}` line and
insert the host-module load before `s.install(ctx)`:

```go
	s := &wasmState{
		rt: env.Runtime(), env: env, wr: wr, realm: env.Raw().Realm(),
		memoryLimitPages: pages,
		wasi:             opts.WASI,
		wasiEnabled:      !opts.WASI.Disable,
	}
	if s.wasiEnabled {
		if _, err := wasi_snapshot_preview1.Instantiate(ctx, wr); err != nil {
			_ = wr.Close(ctx)
			return nil, err
		}
	}
```

The returned cleanup already closes `wr`, which closes the WASI host module, so
no separate `api.Closer` bookkeeping is needed.

### Step 3. Host-namespace detection helpers (`wasm.go`)

Add two small methods next to the other `wasmState` helpers:

```go
// isHostNamespace reports whether an import module is provided by the wazero
// host (WASI) rather than by the JavaScript importObject.
func (s *wasmState) isHostNamespace(module string) bool {
	return s.wasiEnabled && module == wasi_snapshot_preview1.ModuleName
}

// usesWASI reports whether the module imports any host-managed namespace.
func (s *wasmState) usesWASI(m *wasmModule) bool {
	if !s.wasiEnabled {
		return false
	}
	for _, imp := range m.info.imports {
		if s.isHostNamespace(imp.module) {
			return true
		}
	}
	return false
}
```

### Step 4. Skip host-managed imports in `prepareImports` (`wasm.go`)

This is the critical change (WAZERO.md §6.2): `prepareImports` currently throws
a `LinkError` for every import whose module is missing from `importObject`, so
WASI imports would fail before wazero's store lookup ever runs.

In the `for i, imp := range m.info.imports` loop, add a skip at the top that
still advances `fi` for function imports, keeping `m.info.imports` and
`m.compiled.ImportedFunctions()` aligned (they are in the same binary order):

```go
	fds := m.compiled.ImportedFunctions()
	fi := 0
	for i, imp := range m.info.imports {
		if s.isHostNamespace(imp.module) {
			if imp.kind == wasmKindFunc {
				fi++
			}
			continue
		}
		if importObject.IsUndefined() || importObject.IsNull() {
			// ... existing missing-import error ...
		}
		// ... existing nsVal/val lookup and switch ...
	}
```

With all host imports skipped, `resolved` contains only JS namespaces and
wazero's store lookup satisfies WASI.

### Step 5. Attach the WASI environment to the guest `ModuleConfig` (`wasm.go`)

In `instantiate`, replace the inline
`wazero.NewModuleConfig().WithName(name).WithStartFunctions()` with a config
variable that carries the WASI environment only when the module uses WASI
(WAZERO.md §5, §6.4):

```go
	name := s.nextName("instance")
	mc := wazero.NewModuleConfig().WithName(name).WithStartFunctions()
	if s.usesWASI(m) {
		mc = mc.WithArgs(s.wasi.Args...)
		for _, k := range sortedKeys(s.wasi.Env) {
			mc = mc.WithEnv(k, s.wasi.Env[k])
		}
		if s.wasi.FS != nil {
			mc = mc.WithFS(s.wasi.FS)
		}
		if s.wasi.Stdin != nil {
			mc = mc.WithStdin(s.wasi.Stdin)
		}
		if s.wasi.Stdout != nil {
			mc = mc.WithStdout(s.wasi.Stdout)
		}
		if s.wasi.Stderr != nil {
			mc = mc.WithStderr(s.wasi.Stderr)
		}
		if s.wasi.Rand != nil {
			mc = mc.WithRandSource(s.wasi.Rand)
		}
	}
	mod, err := s.wr.InstantiateModule(ictx, m.compiled, mc)
```

> `sortedKeys` already exists in `modules.go`; use it for deterministic
> `WithEnv` application (map iteration order is otherwise random).

Run the verification commands; Phase A should build and the untagged build must
still compile (`wasm_off.go` only reads `Disable`/`MemoryLimitPages`).

---

## Phase B — `_start` execution (optional, recommended)

### Step 6. `RunStart` + `proc_exit` mapping

If `s.wasi.RunStart` is true and the module uses WASI, run `_start` right after
instantiation in `instantiate`, before building the JS exports, and translate
`*sys.ExitError` (WAZERO.md §4.4). Add the import
`"github.com/tetratelabs/wazero/sys"`.

```go
	if s.wasiEnabled && s.wasi.RunStart {
		if start := mod.ExportedFunction("_start"); start != nil {
			if _, err := start.Call(ctx); err != nil {
				var exitErr *sys.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 0 {
					return nil, s.throwError(s.runtimeErrClass, err.Error())
				}
				// proc_exit(0) is success; the module is now closed.
			}
		}
	}
```

Notes:
- After `proc_exit`, the guest `api.Module` is closed; only `RunStart` command
  modules are affected, and their exports are typically unused afterward.
- `errors` is already imported in `wasm.go`? — verify; it is not, add it.
- Optional follow-ups (out of scope for now): `wasi_unstable` alias via
  `wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(builder)`
  instantiated under `"wasi_unstable"`; `env`/`assemblyscript`/`emscripten`
  namespaces.

---

## Phase C — documentation

### Step 7. Update docs

- `doc.go` (WebAssembly section, ~lines 102-122): replace "no WASI, no host
  file system, no environment or stdio" with a description of WASI support and
  the new `Config.WebAssembly.WASI` options.
- `README.md` (WebAssembly section, ~lines 283-332): same — document that
  `wasi_snapshot_preview1` is auto-provided, list `WASI` options, and keep the
  note that `_start` is not run automatically (unless `RunStart`).
- `wasm_off.go`: optionally extend the no-tag guard to reject a non-empty
  `opts.WASI` (mirroring the `MemoryLimitPages` guard), or add a comment that
  WASI options are ignored without the tag. Keep it minimal.

---

## Phase D — tests

### Step 8. Add `wasm_wasi_test.go` (build-tagged `canopy_wasm`)

Reuse the existing binary-builder helpers in `wasm_test.go`
(`testModule`, `testTypeSection`, `testImportSection`, `testFunctionSection`,
`testMemorySection`, `testExportSection`, `testCodeSection`) to build a minimal
WASI module in-process — no TinyGo/wat2wasm needed.

```go
//go:build canopy_wasm

package js

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

// testWASIModule imports wasi_snapshot_preview1.args_get, exports memory and a
// _start that calls args_get(0,0) and drops the result.
func testWASIModule() []byte {
	return testModule(
		testTypeSection(wasmFuncSig{
			params:  []api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			results: []api.ValueType{api.ValueTypeI32},
		}),
		testImportSection(wasmImportSpec{
			module: "wasi_snapshot_preview1", name: "args_get",
			kind: wasmKindFunc, typeIndex: 0,
		}),
		testFunctionSection(0),
		testMemorySection(wasmLimits{min: 1}),
		testExportSection(
			wasmExportSpec{name: "memory", kind: wasmKindMemory, index: 0},
			wasmExportSpec{name: "_start", kind: wasmKindFunc, index: 1},
		),
		testCodeSection([]byte{0x00, 0x41, 0x00, 0x41, 0x00, 0x10, 0x00, 0x1a, 0x0b}),
	)
}

func TestWebAssemblyWASIAutoInstantiate(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {});
  instance.exports._start();
  return "ok";
}`)
	got, err := ex.Call(context.Background(), "run", testWASIModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "ok" {
		t.Fatalf("run = %#v, want %q", got, "ok")
	}
}
```

Add a second test that guards the `fi`-index alignment (WAZERO.md §6.2/§7): a
module importing **both** a WASI function and a JS `env` function:

```go
// testWASIMixedModule imports wasi args_get (func 0) and env.double (func 1);
// _start calls both and drops the results.
func testWASIMixedModule() []byte {
	return testModule(
		testTypeSection(
			wasmFuncSig{params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}, // 0: args_get
			wasmFuncSig{params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}},                 // 1: double
			wasmFuncSig{}, // 2: _start
		),
		testImportSection(
			wasmImportSpec{module: "wasi_snapshot_preview1", name: "args_get", kind: wasmKindFunc, typeIndex: 0},
			wasmImportSpec{module: "env", name: "double", kind: wasmKindFunc, typeIndex: 1},
		),
		testFunctionSection(2),
		testMemorySection(wasmLimits{min: 1}),
		testExportSection(
			wasmExportSpec{name: "memory", kind: wasmKindMemory, index: 0},
			wasmExportSpec{name: "_start", kind: wasmKindFunc, index: 2},
		),
		testCodeSection([]byte{
			0x00,                   // 0 locals
			0x41, 0x00, 0x41, 0x00, // i32.const 0; i32.const 0
			0x10, 0x00, 0x1a,       // call args_get; drop
			0x41, 0x15, 0x10, 0x01, 0x1a, // i32.const 21; call double; drop
			0x0b,                   // end
		}),
	)
}

func TestWebAssemblyWASIMixedImports(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {
    env: { double: (x) => x * 2 },
  });
  instance.exports._start();
  return "ok";
}`)
	got, err := ex.Call(context.Background(), "run", testWASIMixedModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "ok" {
		t.Fatalf("run = %#v, want %q", got, "ok")
	}
}
```

> If a bytecode byte is off, the compile/instantiate step fails; adjust the
> body bytes and re-run `go test -tags canopy_wasm ./...` until green. Both
> tests fail on the current `main` (LinkError), which is the point.

If Step 6 (`RunStart`) was implemented, add one more test that sets
`WebAssembly.WASI{Args: []string{"a","b"}, RunStart: true}` on the `Config`
(use `New(Config{...})` directly instead of `wasmTestExec`) and asserts `_start`
runs without error. Keep it out if `RunStart` is skipped.

---

## Final verification

```sh
go build ./...
go build -tags canopy_wasm ./...
go vet -tags canopy_wasm ./...
go test ./...
go test -tags canopy_wasm ./...
```

All must pass. Confirm `git diff` shows only the intended files
(`wasm_options.go`, `wasm.go`, `wasm_wasi_test.go`, `doc.go`, `README.md`,
optionally `wasm_off.go`).

## Risks / notes (carry forward)

- `prepareImports` skipping (Step 4) is the make-or-break change; without it,
  the host module alone does nothing (WAZERO.md §7).
- Guest `sys.Context` comes from the guest's own `ModuleConfig`; forgetting
  Step 5 leaves WASI imports resolved but with an empty environment.
- `proc_exit` closes the guest; only relevant once `RunStart` is enabled.
- WASI defaults are deliberately sandboxed (deterministic `random_get`, no
  clocks/FS/stdio) unless the corresponding option is set (WAZERO.md §4.3).
- One `wasi_snapshot_preview1` instance per runtime; the eager load in Step 2
  is per-`wasmState` and runs on every pooled runtime (acceptable; a `sync.Once`
  lazy load is the documented alternative if that cost matters).
