//go:build canopy_wasm && !(js && wasm)

package js

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

// testWASITypeSections are shared by the hand-assembled WASI modules below:
// args_get(i32, i32) -> i32 and _start() -> ().
var (
	testWASIArgsGetSig = wasmFuncSig{
		params:  []api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
		results: []api.ValueType{api.ValueTypeI32},
	}
	testWASIStartSig = wasmFuncSig{}
)

// testWASIModule imports wasi_snapshot_preview1.args_get, exports memory and a
// _start that calls args_get(0, 16) and drops the result.
func testWASIModule() []byte {
	return testModule(
		testTypeSection(testWASIArgsGetSig, testWASIStartSig),
		testImportSection(wasmImportSpec{
			module: "wasi_snapshot_preview1", name: "args_get",
			kind: wasmKindFunc, typeIndex: 0,
		}),
		testFunctionSection(1),
		testMemorySection(wasmLimits{min: 1}),
		testExportSection(
			wasmExportSpec{name: "memory", kind: wasmKindMemory, index: 0},
			wasmExportSpec{name: "_start", kind: wasmKindFunc, index: 1},
		),
		testCodeSection([]byte{
			0x00,                   // 0 locals
			0x41, 0x00, 0x41, 0x10, // i32.const 0; i32.const 16
			0x10, 0x00, 0x1a, // call args_get; drop
			0x0b, // end
		}),
	)
}

// testWASIMixedModule imports wasi args_get (func 0) and env.double (func 1);
// _start calls both and drops the results.
func testWASIMixedModule() []byte {
	return testModule(
		testTypeSection(
			testWASIArgsGetSig, // 0: args_get
			wasmFuncSig{
				params:  []api.ValueType{api.ValueTypeI32},
				results: []api.ValueType{api.ValueTypeI32},
			}, // 1: double
			testWASIStartSig, // 2: _start
		),
		testImportSection(
			wasmImportSpec{
				module: "wasi_snapshot_preview1", name: "args_get",
				kind: wasmKindFunc, typeIndex: 0,
			},
			wasmImportSpec{
				module: "env", name: "double",
				kind: wasmKindFunc, typeIndex: 1,
			},
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
			0x10, 0x00, 0x1a, // call args_get; drop
			0x41, 0x15, 0x10, 0x01, 0x1a, // i32.const 21; call double; drop
			0x0b, // end
		}),
	)
}

// TestWebAssemblyWASIAutoInstantiate is the core promise of the feature: a
// module that imports wasi_snapshot_preview1 instantiates and runs with an
// empty JavaScript importObject.
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

// TestWebAssemblyWASIMixedImports guards the imported-function index
// alignment: skipping the WASI import must not shift the JS env function.
func TestWebAssemblyWASIMixedImports(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  let seen = 0;
  const { instance } = await WebAssembly.instantiate(bytes, {
    env: { double: (x) => { seen = x * 2; return x * 2; } },
  });
  instance.exports._start();
  return seen;
}`)
	got, err := ex.Call(context.Background(), "run", testWASIMixedModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != int64(42) {
		t.Fatalf("run = %#v, want %d", got, 42)
	}
}

// TestWebAssemblyWASIArgs checks that WebAssembly.WASI.Args reaches the
// guest's sys.Context: args_get writes argv pointers at offset 0 and the
// null-terminated argument strings at offset 16.
func TestWebAssemblyWASIArgs(t *testing.T) {
	ex, err := New(Config{
		Name: "wasm-wasi-args.js",
		Source: `
export async function run(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {});
  instance.exports._start();
  const mem = new Uint8Array(instance.exports.memory.buffer);
  const p = mem[0] | mem[1] << 8 | mem[2] << 16 | mem[3] << 24;
  return [p, mem[p], mem[p + 1], mem[p + 2], mem[p + 3]];
}`,
		WebAssembly: WebAssemblyOptions{
			WASI: WebAssemblyWASIOptions{Args: []string{"app", "arg"}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := ex.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	got, err := ex.Call(context.Background(), "run", testWASIModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, "[16,97,112,112,0]") // argv[0] -> offset 16, "app\0"
}

// TestWebAssemblyWASIRunStart checks that RunStart executes _start during
// instantiation, before JavaScript receives the instance.
func TestWebAssemblyWASIRunStart(t *testing.T) {
	ex, err := New(Config{
		Name: "wasm-wasi-run-start.js",
		Source: `
export async function run(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {});
  const mem = new Uint8Array(instance.exports.memory.buffer);
  const p = mem[0] | mem[1] << 8 | mem[2] << 16 | mem[3] << 24;
  return [p, mem[p], mem[p + 1], mem[p + 2], mem[p + 3]];
}`,
		WebAssembly: WebAssemblyOptions{
			WASI: WebAssemblyWASIOptions{
				Args:     []string{"app", "arg"},
				RunStart: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := ex.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	got, err := ex.Call(context.Background(), "run", testWASIModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, "[16,97,112,112,0]")
}

// testWASIExitModule imports proc_exit and calls it with code in _start.
func testWASIExitModule(code byte) []byte {
	return testModule(
		testTypeSection(
			wasmFuncSig{params: []api.ValueType{api.ValueTypeI32}}, // 0: proc_exit
			testWASIStartSig, // 1: _start
		),
		testImportSection(wasmImportSpec{
			module: "wasi_snapshot_preview1", name: "proc_exit",
			kind: wasmKindFunc, typeIndex: 0,
		}),
		testFunctionSection(1),
		testExportSection(wasmExportSpec{name: "_start", kind: wasmKindFunc, index: 1}),
		testCodeSection([]byte{
			0x00,       // 0 locals
			0x41, code, // i32.const code
			0x10, 0x00, // call proc_exit
			0x0b, // end
		}),
	)
}

// TestWebAssemblyWASIRunStartExitZero treats proc_exit(0) as success.
func TestWebAssemblyWASIRunStartExitZero(t *testing.T) {
	ex, err := New(Config{
		Name: "wasm-wasi-exit-zero.js",
		Source: `
export async function run(bytes) {
  await WebAssembly.instantiate(bytes, {});
  return "ok";
}`,
		WebAssembly: WebAssemblyOptions{
			WASI: WebAssemblyWASIOptions{RunStart: true},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := ex.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	got, err := ex.Call(context.Background(), "run", testWASIExitModule(0))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "ok" {
		t.Fatalf("run = %#v, want %q", got, "ok")
	}
}

// TestWebAssemblyWASIRunStartExitNonZero surfaces a non-zero proc_exit as a
// WebAssembly.RuntimeError.
func TestWebAssemblyWASIRunStartExitNonZero(t *testing.T) {
	ex, err := New(Config{
		Name: "wasm-wasi-exit-nonzero.js",
		Source: `
export async function run(bytes) {
  await WebAssembly.instantiate(bytes, {});
  return "ok";
}`,
		WebAssembly: WebAssemblyOptions{
			WASI: WebAssemblyWASIOptions{RunStart: true},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := ex.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if _, err := ex.Call(context.Background(), "run", testWASIExitModule(3)); err == nil {
		t.Fatalf("run with proc_exit(3) returned no error")
	}
}

// TestWebAssemblyWASIDisable checks the opt-out: without the host module, the
// unresolved import is a LinkError again.
func TestWebAssemblyWASIDisable(t *testing.T) {
	ex, err := New(Config{
		Name: "wasm-wasi-disabled.js",
		Source: `
export async function run(bytes) {
  try {
    await WebAssembly.instantiate(bytes, {});
    return "instantiated";
  } catch (e) {
    return e.constructor.name;
  }
}`,
		WebAssembly: WebAssemblyOptions{
			WASI: WebAssemblyWASIOptions{Disable: true},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := ex.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	got, err := ex.Call(context.Background(), "run", testWASIModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "LinkError" {
		t.Fatalf("run = %#v, want %q", got, "LinkError")
	}
}
