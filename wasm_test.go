//go:build !nowasm && !(js && wasm)

package jolt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/MoeForge/moejs"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func wasmTestExec(t *testing.T, source string) *Executor {
	t.Helper()
	ex, err := New(Config{Name: "wasm-test.js", Source: source})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := ex.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return ex
}

func wantJSON(t *testing.T, got any, want string) {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if string(b) != want {
		t.Fatalf("got %s, want %s", b, want)
	}
}

func testModule(sections ...[]byte) []byte {
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	for _, s := range sections {
		out = append(out, s...)
	}
	return out
}

func testTypeSection(sigs ...wasmFuncSig) []byte {
	var p []byte
	p = appendU32(p, uint32(len(sigs)))
	for _, s := range sigs {
		p = append(p, 0x60)
		p = appendU32(p, uint32(len(s.params)))
		for _, t := range s.params {
			p = append(p, byte(t))
		}
		p = appendU32(p, uint32(len(s.results)))
		for _, t := range s.results {
			p = append(p, byte(t))
		}
	}
	return wasmSection(1, p)
}

func testFunctionSection(types ...uint32) []byte {
	p := appendU32(nil, uint32(len(types)))
	for _, t := range types {
		p = appendU32(p, t)
	}
	return wasmSection(3, p)
}

func testImportSection(imports ...wasmImportSpec) []byte {
	p := appendU32(nil, uint32(len(imports)))
	for _, imp := range imports {
		p = appendString(p, imp.module)
		p = appendString(p, imp.name)
		p = append(p, byte(imp.kind))
		switch imp.kind {
		case wasmKindFunc:
			p = appendU32(p, imp.typeIndex)
		case wasmKindMemory:
			p = appendLimits(p, imp.limits)
		case wasmKindGlobal:
			p = append(p, byte(imp.valueType), boolByte(imp.mutable))
		}
	}
	return wasmSection(2, p)
}

func testExportSection(exports ...wasmExportSpec) []byte {
	p := appendU32(nil, uint32(len(exports)))
	for _, e := range exports {
		p = appendString(p, e.name)
		p = append(p, byte(e.kind))
		p = appendU32(p, e.index)
	}
	return wasmSection(7, p)
}

func testMemorySection(limits ...wasmLimits) []byte {
	p := appendU32(nil, uint32(len(limits)))
	for _, l := range limits {
		p = appendLimits(p, l)
	}
	return wasmSection(5, p)
}

func testGlobalSection(globals ...wasmGlobalSpec) []byte {
	p := appendU32(nil, uint32(len(globals)))
	for _, g := range globals {
		p = append(p, byte(g.valueType), boolByte(g.mutable))
		p = append(p, g.init...)
		p = append(p, 0x0b)
	}
	return wasmSection(6, p)
}

func testCodeSection(bodies ...[]byte) []byte {
	p := appendU32(nil, uint32(len(bodies)))
	for _, body := range bodies {
		p = appendU32(p, uint32(len(body)))
		p = append(p, body...)
	}
	return wasmSection(10, p)
}

func testCustomSection(name string, data []byte) []byte {
	p := appendString(nil, name)
	p = append(p, data...)
	return wasmSection(0, p)
}

func testStartSection(index uint32) []byte {
	return wasmSection(8, appendU32(nil, index))
}

func testAddModule() []byte {
	return testModule(
		testTypeSection(wasmFuncSig{
			params:  []api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			results: []api.ValueType{api.ValueTypeI32},
		}),
		testFunctionSection(0),
		testExportSection(wasmExportSpec{name: "add", kind: wasmKindFunc, index: 0}),
		testCodeSection([]byte{0x00, 0x20, 0x00, 0x20, 0x01, 0x6a, 0x0b}),
		testCustomSection("meta", []byte("hello")),
	)
}

func testMemoryModule() []byte {
	return testModule(
		testTypeSection(
			wasmFuncSig{params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}},
			wasmFuncSig{params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}},
		),
		testFunctionSection(0, 1),
		testMemorySection(wasmLimits{min: 1}),
		testExportSection(
			wasmExportSpec{name: "memory", kind: wasmKindMemory, index: 0},
			wasmExportSpec{name: "store", kind: wasmKindFunc, index: 0},
			wasmExportSpec{name: "load", kind: wasmKindFunc, index: 1},
		),
		testCodeSection(
			[]byte{0x00, 0x20, 0x00, 0x20, 0x01, 0x36, 0x02, 0x00, 0x0b},
			[]byte{0x00, 0x20, 0x00, 0x28, 0x02, 0x00, 0x0b},
		),
	)
}

func testImportFuncModule() []byte {
	return testModule(
		testTypeSection(wasmFuncSig{
			params:  []api.ValueType{api.ValueTypeI32},
			results: []api.ValueType{api.ValueTypeI32},
		}),
		testImportSection(wasmImportSpec{
			module: "env", name: "double", kind: wasmKindFunc, typeIndex: 0,
		}),
		testFunctionSection(0),
		testExportSection(wasmExportSpec{name: "run", kind: wasmKindFunc, index: 1}),
		testCodeSection([]byte{0x00, 0x20, 0x00, 0x10, 0x00, 0x0b}),
	)
}

func testImportMemoryModule() []byte {
	return testModule(
		testTypeSection(
			wasmFuncSig{params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}},
			wasmFuncSig{params: []api.ValueType{api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}},
		),
		testImportSection(wasmImportSpec{
			module: "env", name: "memory", kind: wasmKindMemory,
			limits: wasmLimits{min: 1},
		}),
		testFunctionSection(0, 1),
		testExportSection(
			wasmExportSpec{name: "store", kind: wasmKindFunc, index: 0},
			wasmExportSpec{name: "load", kind: wasmKindFunc, index: 1},
		),
		testCodeSection(
			[]byte{0x00, 0x20, 0x00, 0x20, 0x01, 0x36, 0x02, 0x00, 0x0b},
			[]byte{0x00, 0x20, 0x00, 0x28, 0x02, 0x00, 0x0b},
		),
	)
}

func testImportGlobalModule() []byte {
	return testModule(
		testTypeSection(
			wasmFuncSig{results: []api.ValueType{api.ValueTypeI32}},
			wasmFuncSig{params: []api.ValueType{api.ValueTypeI32}},
		),
		testImportSection(wasmImportSpec{
			module: "env", name: "g", kind: wasmKindGlobal,
			valueType: api.ValueTypeI32, mutable: true,
		}),
		testFunctionSection(0, 1),
		testExportSection(
			wasmExportSpec{name: "get", kind: wasmKindFunc, index: 0},
			wasmExportSpec{name: "set", kind: wasmKindFunc, index: 1},
		),
		testCodeSection(
			[]byte{0x00, 0x23, 0x00, 0x0b},
			[]byte{0x00, 0x20, 0x00, 0x24, 0x00, 0x0b},
		),
	)
}

func testGlobalModule() []byte {
	return testModule(
		testTypeSection(wasmFuncSig{results: []api.ValueType{api.ValueTypeI32}}),
		testFunctionSection(0),
		testGlobalSection(wasmGlobalSpec{
			valueType: api.ValueTypeI32, mutable: true, init: wasmConstI32(5),
		}),
		testExportSection(
			wasmExportSpec{name: "g", kind: wasmKindGlobal, index: 0},
			wasmExportSpec{name: "get", kind: wasmKindFunc, index: 0},
		),
		testCodeSection([]byte{0x00, 0x23, 0x00, 0x0b}),
	)
}

func testTrapModule() []byte {
	return testModule(
		testTypeSection(wasmFuncSig{}),
		testFunctionSection(0),
		testExportSection(wasmExportSpec{name: "boom", kind: wasmKindFunc, index: 0}),
		testCodeSection([]byte{0x00, 0x00, 0x0b}),
	)
}

func testStartTrapModule() []byte {
	return testModule(
		testTypeSection(wasmFuncSig{}),
		testFunctionSection(0),
		testStartSection(0),
		testCodeSection([]byte{0x00, 0x00, 0x0b}),
	)
}

func testMultiValueModule() []byte {
	return testModule(
		testTypeSection(wasmFuncSig{results: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}}),
		testFunctionSection(0),
		testExportSection(wasmExportSpec{name: "pair", kind: wasmKindFunc, index: 0}),
		testCodeSection([]byte{0x00, 0x41, 0x01, 0x41, 0x02, 0x0b}),
	)
}

func TestWebAssemblyCompileInstantiate(t *testing.T) {
	ex := wasmTestExec(t, `
export async function add(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {});
  return instance.exports.add(2, 3);
}`)
	got, err := ex.Call(context.Background(), "add", testAddModule())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if got != int64(5) {
		t.Fatalf("add = %#v, want 5", got)
	}
}

func TestWebAssemblyModuleInstanceValidate(t *testing.T) {
	ex := wasmTestExec(t, `
export function run(bytes, invalid) {
  const m = new WebAssembly.Module(bytes);
  const i = new WebAssembly.Instance(m, {});
  return [
    m instanceof WebAssembly.Module,
    i instanceof WebAssembly.Instance,
    i.exports.add(20, 22),
    WebAssembly.validate(bytes),
    WebAssembly.validate(invalid),
  ];
}`)
	got, err := ex.Call(context.Background(), "run", testAddModule(), []byte{0, 1, 2})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[true,true,42,true,false]`)
}

func TestWebAssemblyErrors(t *testing.T) {
	ex := wasmTestExec(t, `
export function compileError(bytes) {
  try {
    new WebAssembly.Module(bytes);
    return "no error";
  } catch (e) {
    return [e instanceof WebAssembly.CompileError, e instanceof Error, e.name, e.message.length > 0];
  }
}
export async function linkError(bytes) {
  try {
    await WebAssembly.instantiate(bytes, {});
    return "no error";
  } catch (e) {
    return [e instanceof WebAssembly.LinkError, e instanceof Error, e.name, e.message.length > 0];
  }
}
export async function runtimeError(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {});
  try {
    instance.exports.boom();
    return "no error";
  } catch (e) {
    return [e instanceof WebAssembly.RuntimeError, e instanceof Error, e.name, e.message.length > 0];
  }
}
export async function startError(bytes) {
  try {
    await WebAssembly.instantiate(bytes, {});
    return "no error";
  } catch (e) {
    return [e instanceof WebAssembly.RuntimeError, e.name];
  }
}`)
	ctx := context.Background()
	if got, err := ex.Call(ctx, "compileError", []byte{0, 1, 2}); err != nil {
		t.Fatalf("compileError: %v", err)
	} else {
		wantJSON(t, got, `[true,true,"CompileError",true]`)
	}
	if got, err := ex.Call(ctx, "linkError", testImportFuncModule()); err != nil {
		t.Fatalf("linkError: %v", err)
	} else {
		wantJSON(t, got, `[true,true,"LinkError",true]`)
	}
	if got, err := ex.Call(ctx, "runtimeError", testTrapModule()); err != nil {
		t.Fatalf("runtimeError: %v", err)
	} else {
		wantJSON(t, got, `[true,true,"RuntimeError",true]`)
	}
	if got, err := ex.Call(ctx, "startError", testStartTrapModule()); err != nil {
		t.Fatalf("startError: %v", err)
	} else {
		wantJSON(t, got, `[true,"RuntimeError"]`)
	}
}

func TestWebAssemblyFunctionImports(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const module = new WebAssembly.Module(bytes);
  const good = new WebAssembly.Instance(module, { env: { double(n) { return n * 2; } } });
  const a = good.exports.run(21);

  const bad = new WebAssembly.Instance(module, { env: { double() { throw new TypeError("js boom"); } } });
  let caught = null;
  try { bad.exports.run(1); } catch (e) { caught = [e instanceof TypeError, e.message]; }
  return [a, caught[0], caught[1]];
}`)
	got, err := ex.Call(context.Background(), "run", testImportFuncModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[42,true,"js boom"]`)
}

func TestWebAssemblyMemory(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {});
  const mem = instance.exports.memory;
  const view = new Uint8Array(mem.buffer);
  instance.exports.store(0, 0x11223344);
  const seen = [view[0], view[1], view[2], view[3]];
  view[8] = 7;
  const loaded = instance.exports.load(8);
  const prev = mem.grow(1);
  return [seen[0], seen[3], loaded, prev, mem.buffer.byteLength];
}`)
	got, err := ex.Call(context.Background(), "run", testMemoryModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[68,17,7,1,131072]`)
}

func TestWebAssemblyImportedMemory(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const mem = new WebAssembly.Memory({ initial: 1, maximum: 2 });
  const { instance } = await WebAssembly.instantiate(bytes, { env: { memory: mem } });
  new Uint8Array(mem.buffer)[0] = 42;
  const loaded = instance.exports.load(0);
  instance.exports.store(4, 99);
  const seen = new Uint8Array(mem.buffer)[4];
  return [loaded, seen, mem instanceof WebAssembly.Memory];
}`)
	got, err := ex.Call(context.Background(), "run", testImportMemoryModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[42,99,true]`)
}

func TestWebAssemblyExportedMemoryReimport(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(memoryBytes, importBytes) {
  const a = (await WebAssembly.instantiate(memoryBytes, {})).instance;
  const b = (await WebAssembly.instantiate(importBytes, { env: { memory: a.exports.memory } })).instance;
  b.exports.store(0, 55);
  return [new Uint8Array(a.exports.memory.buffer)[0], b.exports.load(0)];
}`)
	got, err := ex.Call(context.Background(), "run", testMemoryModule(), testImportMemoryModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[55,55]`)
}

func TestWebAssemblyGlobalImport(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const g = new WebAssembly.Global({ value: "i32", mutable: true }, 7);
  const { instance } = await WebAssembly.instantiate(bytes, { env: { g } });
  const before = instance.exports.get();
  instance.exports.set(9);
  const after = g.value;
  g.value = 11;
  return [before, after, instance.exports.get(), g instanceof WebAssembly.Global];
}`)
	got, err := ex.Call(context.Background(), "run", testImportGlobalModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[7,9,11,true]`)
}

func TestWebAssemblyGlobalExport(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {});
  const g = instance.exports.g;
  const before = g.value;
  g.value = 6;
  return [before, instance.exports.get(), g instanceof WebAssembly.Global];
}`)
	got, err := ex.Call(context.Background(), "run", testGlobalModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[5,6,true]`)
}

func TestWebAssemblyStreamingAndIntrospection(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const response = { arrayBuffer: () => Promise.resolve(bytes) };
  const module = await WebAssembly.compileStreaming(response);
  const instance = await WebAssembly.instantiate(module, {});
  const exports = WebAssembly.Module.exports(module);
  return [
    instance.exports.add(1, 2),
    module instanceof WebAssembly.Module,
    exports.length,
    exports[0].name,
    exports[0].kind,
  ];
}
export async function streaming(bytes) {
  const response = { arrayBuffer: () => Promise.resolve(bytes) };
  const result = await WebAssembly.instantiateStreaming(response, {});
  return [result.module instanceof WebAssembly.Module, result.instance.exports.add(2, 2)];
}`)
	ctx := context.Background()
	if got, err := ex.Call(ctx, "run", testAddModule()); err != nil {
		t.Fatalf("run: %v", err)
	} else {
		wantJSON(t, got, `[3,true,1,"add","function"]`)
	}
	if got, err := ex.Call(ctx, "streaming", testAddModule()); err != nil {
		t.Fatalf("streaming: %v", err)
	} else {
		wantJSON(t, got, `[true,4]`)
	}
}

func TestWebAssemblyImportsIntrospection(t *testing.T) {
	ex := wasmTestExec(t, `
export function run(bytes) {
  const m = new WebAssembly.Module(bytes);
  const im = WebAssembly.Module.imports(m);
  return [im.length, im[0].module, im[0].name, im[0].kind];
}`)
	got, err := ex.Call(context.Background(), "run", testImportFuncModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[1,"env","double","function"]`)
}

func TestWebAssemblyCustomSections(t *testing.T) {
	ex := wasmTestExec(t, `
export function run(bytes) {
  const m = new WebAssembly.Module(bytes);
  const sections = WebAssembly.Module.customSections(m, "meta");
  return [sections.length, new TextDecoder().decode(sections[0])];
}`)
	got, err := ex.Call(context.Background(), "run", testAddModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[1,"hello"]`)
}

func TestWebAssemblyMultiValue(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {});
  return instance.exports.pair();
}`)
	got, err := ex.Call(context.Background(), "run", testMultiValueModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[1,2]`)
}

func TestWebAssemblyBadArguments(t *testing.T) {
	ex := wasmTestExec(t, `
export function run() {
  const out = [];
  try { WebAssembly.compile(); } catch (e) { out.push(e instanceof TypeError); }
  try { new WebAssembly.Memory({}); } catch (e) { out.push(e instanceof TypeError); }
  const immutable = new WebAssembly.Global({ value: "i32" }, 1);
  try { immutable.value = 2; } catch (e) { out.push(e instanceof TypeError); }
  try { new WebAssembly.Global({ value: "v128" }, 1); } catch (e) { out.push(e instanceof TypeError); }
  try { new WebAssembly.Module([1, 2, 3]); } catch (e) { out.push(e instanceof TypeError); }
  return out;
}`)
	got, err := ex.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[true,true,true,true,true]`)
}

func TestWebAssemblyDisabled(t *testing.T) {
	ex, err := New(Config{
		Name:        "wasm-off.js",
		Source:      `export function type() { return typeof WebAssembly; }`,
		WebAssembly: WasmOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ex.Close()
	got, err := ex.Call(context.Background(), "type")
	if err != nil {
		t.Fatalf("type: %v", err)
	}
	if got != "undefined" {
		t.Fatalf("typeof WebAssembly = %#v, want undefined", got)
	}
}

func testNumericModule() []byte {
	return testModule(
		testTypeSection(
			wasmFuncSig{
				params:  []api.ValueType{api.ValueTypeI64, api.ValueTypeI64},
				results: []api.ValueType{api.ValueTypeI64},
			},
			wasmFuncSig{
				params:  []api.ValueType{api.ValueTypeF64, api.ValueTypeF64},
				results: []api.ValueType{api.ValueTypeF64},
			},
			wasmFuncSig{
				params:  []api.ValueType{api.ValueTypeF32, api.ValueTypeF32},
				results: []api.ValueType{api.ValueTypeF32},
			},
		),
		testFunctionSection(0, 1, 2),
		testExportSection(
			wasmExportSpec{name: "add64", kind: wasmKindFunc, index: 0},
			wasmExportSpec{name: "addf64", kind: wasmKindFunc, index: 1},
			wasmExportSpec{name: "addf32", kind: wasmKindFunc, index: 2},
		),
		testCodeSection(
			[]byte{0x00, 0x20, 0x00, 0x20, 0x01, 0x7c, 0x0b},
			[]byte{0x00, 0x20, 0x00, 0x20, 0x01, 0xa0, 0x0b},
			[]byte{0x00, 0x20, 0x00, 0x20, 0x01, 0x92, 0x0b},
		),
	)
}

func TestWebAssemblyNumericTypes(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {});
  return [
    instance.exports.add64(2n, 3n).toString(),
    instance.exports.add64(-2n, 3n).toString(),
    instance.exports.addf64(1.5, 2.25),
    instance.exports.addf32(1.5, 1.75),
  ];
}`)
	got, err := ex.Call(context.Background(), "run", testNumericModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `["5","1",3.75,3.25]`)
}

func TestWebAssemblyTableShape(t *testing.T) {
	ex := wasmTestExec(t, `
export function run() {
  const t = new WebAssembly.Table({ element: "anyfunc", initial: 2 });
  let threw = false;
  try { t.get(0); } catch (e) { threw = e instanceof TypeError; }
  return [t.length, t instanceof WebAssembly.Table, threw];
}
export function noNew(bytes) {
  try { WebAssembly.Module(bytes); return false; } catch (e) { return e instanceof TypeError; }
}`)
	ctx := context.Background()
	if got, err := ex.Call(ctx, "run"); err != nil {
		t.Fatalf("run: %v", err)
	} else {
		wantJSON(t, got, `[2,true,true]`)
	}
	if got, err := ex.Call(ctx, "noNew", testAddModule()); err != nil {
		t.Fatalf("noNew: %v", err)
	} else if got != true {
		t.Fatalf("noNew = %#v, want true", got)
	}
}

func testImportMixedModule() []byte {
	return testModule(
		testTypeSection(wasmFuncSig{
			params:  []api.ValueType{api.ValueTypeI32},
			results: []api.ValueType{api.ValueTypeI32},
		}),
		testImportSection(
			wasmImportSpec{module: "env", name: "memory", kind: wasmKindMemory, limits: wasmLimits{min: 1}},
			wasmImportSpec{module: "env", name: "double", kind: wasmKindFunc, typeIndex: 0},
		),
		testFunctionSection(0),
		testExportSection(wasmExportSpec{name: "run", kind: wasmKindFunc, index: 1}),
		testCodeSection([]byte{
			0x00, 0x20, 0x00, 0x10, 0x00, 0x41, 0x00, 0x28, 0x02, 0x00, 0x6a, 0x0b,
		}),
	)
}

func TestWebAssemblyMixedImports(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const mem = new WebAssembly.Memory({ initial: 1 });
  new Uint8Array(mem.buffer)[0] = 5;
  const { instance } = await WebAssembly.instantiate(bytes, {
    env: { memory: mem, double(n) { return n * 2; } },
  });
  return instance.exports.run(3);
}`)
	got, err := ex.Call(context.Background(), "run", testImportMixedModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != int64(11) {
		t.Fatalf("run = %#v, want 11", got)
	}
}

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
		WebAssembly: WasmOptions{
			WASI: WasiOptions{Args: []string{"app", "arg"}},
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
		WebAssembly: WasmOptions{
			WASI: WasiOptions{
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
		WebAssembly: WasmOptions{
			WASI: WasiOptions{RunStart: true},
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
		WebAssembly: WasmOptions{
			WASI: WasiOptions{RunStart: true},
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
		WebAssembly: WasmOptions{
			WASI: WasiOptions{Disable: true},
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

// ---------------------------------------------------------------------------
// Coverage additions: parser internals, table/global/memory paths, errors.

func testTableSection(tables ...wasmTableInfo) []byte {
	var p []byte
	p = appendU32(p, uint32(len(tables)))
	for _, t := range tables {
		p = append(p, byte(t.elementType))
		p = appendLimits(p, t.limits)
	}
	return wasmSection(4, p)
}

func testTableModule() []byte {
	return testModule(
		testTypeSection(wasmFuncSig{}),
		testFunctionSection(0),
		testTableSection(wasmTableInfo{
			elementType: wasmValueTypeFuncref,
			limits:      wasmLimits{min: 2, max: 4, hasMax: true},
		}),
		testExportSection(
			wasmExportSpec{name: "f", kind: wasmKindFunc, index: 0},
			wasmExportSpec{name: "t", kind: wasmKindTable, index: 0},
		),
		testCodeSection([]byte{0x00, 0x0b}),
	)
}

// testAllImportKinds builds an import section with one import of every kind.
func testAllImportKinds() []byte {
	p := appendU32(nil, 5)
	p = appendString(p, "f")
	p = appendString(p, "fn")
	p = append(p, byte(wasmKindFunc))
	p = appendU32(p, 0)
	p = appendString(p, "t")
	p = appendString(p, "tab")
	p = append(p, byte(wasmKindTable), byte(wasmValueTypeFuncref))
	p = appendLimits(p, wasmLimits{min: 1, max: 2, hasMax: true})
	p = appendString(p, "m")
	p = appendString(p, "mem")
	p = append(p, byte(wasmKindMemory))
	p = appendLimits(p, wasmLimits{min: 1})
	p = appendString(p, "g")
	p = appendString(p, "glob")
	p = append(p, byte(wasmKindGlobal), byte(api.ValueTypeI64), 1)
	p = appendString(p, "tag")
	p = appendString(p, "e")
	p = append(p, byte(wasmKindTag), 0)
	p = appendU32(p, 0)
	return testModule(testTypeSection(wasmFuncSig{}), wasmSection(2, p))
}

func TestCoverageWasmParserInternals(t *testing.T) {
	wantMagic := func(size int) []byte { return []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}[:size] }
	bad := [][]byte{
		nil,
		{1},
		{0x00, 0x61, 0x73, 0x6d, 0x02, 0x00, 0x00, 0x00},
		append(append([]byte{}, wantMagic(8)...), 1, 5),
	}
	for i, b := range bad {
		if _, err := parseWasmBinary(b); err == nil {
			t.Fatalf("bad binary %d accepted", i)
		}
	}

	info, err := parseWasmBinary(testModule(
		testTypeSection(wasmFuncSig{}),
		testFunctionSection(0),
		testTableSection(wasmTableInfo{elementType: wasmValueTypeFuncref, limits: wasmLimits{min: 1}}),
		testMemorySection(wasmLimits{min: 1, max: 2, hasMax: true}),
		testGlobalSection(wasmGlobalSpec{valueType: api.ValueTypeI32, init: wasmConstI32(1)}),
		testExportSection(wasmExportSpec{name: "g", kind: wasmKindGlobal, index: 0}),
		testStartSection(0),
	))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(info.tables) != 1 || len(info.memories) != 1 || len(info.globals) != 1 ||
		len(info.exports) != 1 || !info.hasStart {
		t.Fatalf("parsed info = %+v", info)
	}

	info, err = parseWasmBinary(testAllImportKinds())
	if err != nil {
		t.Fatalf("parse all imports: %v", err)
	}
	if len(info.imports) != 5 || len(info.tables) != 1 || len(info.memories) != 1 || len(info.globals) != 1 {
		t.Fatalf("all imports = %+v", info)
	}
	if info.imports[3].valueType != api.ValueTypeI64 || !info.imports[3].mutable {
		t.Fatalf("global import = %+v", info.imports[3])
	}

	badImport := testModule(testTypeSection(wasmFuncSig{}), wasmSection(2, func() []byte {
		p := appendU32(nil, 1)
		p = appendString(p, "f")
		p = appendString(p, "x")
		return append(p, 0x05)
	}()))
	if _, err := parseWasmBinary(badImport); err == nil {
		t.Fatalf("unknown import kind accepted")
	}

	// Reader bounds and LEB128 limits.
	for _, c := range []struct {
		r   wasmReader
		run func() error
	}{
		{wasmReader{}, func() error { _, err := (&wasmReader{}).u8(); return err }},
		{wasmReader{b: []byte{0x80, 0x80, 0x80, 0x80, 0x80}}, func() error { _, err := (&wasmReader{b: []byte{0x80, 0x80, 0x80, 0x80, 0x80}}).u32(); return err }},
		{wasmReader{b: []byte{0x05, 'a'}}, func() error { _, err := (&wasmReader{b: []byte{0x05, 'a'}}).str(); return err }},
		{wasmReader{b: []byte{0x01}}, func() error { _, err := (&wasmReader{b: []byte{0x01}}).bytes(2); return err }},
		{wasmReader{b: []byte{0x01}}, func() error { _, err := (&wasmReader{b: []byte{0x01}}).limits(); return err }},
	} {
		if err := c.run(); err == nil {
			t.Fatalf("reader error not reported")
		}
	}

	validInit := [][]byte{
		{0x41, 0x00, 0x0b},
		{0x42, 0x00, 0x0b},
		{0x43, 0, 0, 0, 0, 0x0b},
		{0x44, 0, 0, 0, 0, 0, 0, 0, 0, 0x0b},
		{0x23, 0x00, 0x0b},
		{0xd2, 0x00, 0x0b},
		{0xd0, 0x70, 0x0b},
		{0x6a, 0x0b},
	}
	for i, b := range validInit {
		r := wasmReader{b: b}
		if err := r.skipInitExpr(); err != nil {
			t.Fatalf("init expr %d: %v", i, err)
		}
	}
	for _, b := range [][]byte{{0xff}, {0x41}} {
		r := wasmReader{b: b}
		if err := r.skipInitExpr(); err == nil {
			t.Fatalf("bad init expr %v accepted", b)
		}
	}

	for _, sec := range [][]byte{
		wasmSection(2, []byte{1, 0x01, 'm'}),
		wasmSection(4, []byte{1, 0x70}),
		wasmSection(5, []byte{1}),
		wasmSection(6, []byte{1, 0x7f, 0x00}),
		wasmSection(7, []byte{1, 0x01, 'a'}),
	} {
		if _, err := parseWasmBinary(testModule(testTypeSection(wasmFuncSig{}), sec)); err == nil {
			t.Fatalf("truncated section %v accepted", sec)
		}
	}

	for flags, want := range map[byte]wasmLimits{
		0x00: {min: 1},
		0x01: {min: 1, max: 2, hasMax: true},
		0x02: {min: 1, shared: true},
		0x03: {min: 1, max: 2, hasMax: true, shared: true},
	} {
		var b []byte
		b = append(b, flags, 0x01)
		if flags&0x01 != 0 {
			b = append(b, 0x02)
		}
		lim, err := (&wasmReader{b: b}).limits()
		if err != nil || lim != want {
			t.Fatalf("limits(%x) = %+v, %v", flags, lim, err)
		}
	}

	for kind, want := range map[wasmKind]string{
		wasmKindFunc: "function", wasmKindTable: "table", wasmKindMemory: "memory",
		wasmKindGlobal: "global", wasmKindTag: "tag", wasmKind(99): "unknown",
	} {
		if got := wasmKindName(kind); got != want {
			t.Fatalf("wasmKindName(%d) = %q", kind, got)
		}
	}

	small := wasmFuncSig{}
	large := wasmFuncSig{params: make([]api.ValueType, 300)}
	if small.key() == large.key() {
		t.Fatalf("signature keys collide above 255 parameters")
	}

	if b := wasmConstI64(1); len(b) == 0 || b[0] != 0x42 {
		t.Fatalf("wasmConstI64 = %v", b)
	}
	if b := wasmConstF32(1); len(b) != 5 || b[0] != 0x43 {
		t.Fatalf("wasmConstF32 = %v", b)
	}
	if b := wasmConstF64(1); len(b) != 9 || b[0] != 0x44 {
		t.Fatalf("wasmConstF64 = %v", b)
	}
}

func TestCoverageWasmTableExport(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {});
  const t = instance.exports.t;
  const errs = [];
  try { t.get(0); errs.push(false); } catch (e) { errs.push(e instanceof TypeError); }
  try { t.set(0, null); errs.push(false); } catch (e) { errs.push(e instanceof TypeError); }
  try { t.grow(1); errs.push(false); } catch (e) { errs.push(e instanceof TypeError); }
  return [t.length, t instanceof WebAssembly.Table, errs];
}`)
	got, err := ex.Call(context.Background(), "run", testTableModule())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[2,true,[true,true,true]]`)
}

func TestCoverageWasmConstructors(t *testing.T) {
	ex := wasmTestExec(t, `
export function globals() {
  const gi = new WebAssembly.Global({ value: "i64", mutable: true }, 5n);
  gi.value = 7n;
  const gf = new WebAssembly.Global({ value: "f32" }, 1.5);
  const gd = new WebAssembly.Global({ value: "f64" }, 2.5);
  const errors = [];
  try { new WebAssembly.Global({ value: "v128" }, 1); errors.push(false); } catch (e) { errors.push(e instanceof TypeError); }
  try { new WebAssembly.Global(1, 1); errors.push(false); } catch (e) { errors.push(e instanceof TypeError); }
  const fixed = new WebAssembly.Global({ value: "i32" }, 1);
  try { fixed.value = 2; errors.push(false); } catch (e) { errors.push(e instanceof TypeError); }
  return [gi.value.toString(), gf.value, gd.value, errors];
}
export function memories() {
  const errors = [];
  try { new WebAssembly.Memory(); errors.push(false); } catch (e) { errors.push(e instanceof TypeError); }
  try { new WebAssembly.Memory({}); errors.push(false); } catch (e) { errors.push(e instanceof TypeError); }
  try { new WebAssembly.Memory({ initial: 1, shared: true }); errors.push(false); } catch (e) { errors.push(e instanceof TypeError); }
  try { new WebAssembly.Memory({ initial: 1, maximum: 65537 }); errors.push(false); } catch (e) { errors.push(e instanceof RangeError); }
  try { new WebAssembly.Memory({ initial: 2, maximum: 1 }); errors.push(false); } catch (e) { errors.push(e instanceof RangeError); }
  const m = new WebAssembly.Memory({ initial: 1, maximum: 1 });
  const first = m.grow(1);
  const zero = m.grow(0);
  return [errors, first, zero, m.buffer.byteLength];
}
export function tables() {
  const errors = [];
  try { new WebAssembly.Table(); errors.push(false); } catch (e) { errors.push(e instanceof TypeError); }
  try { new WebAssembly.Table({ element: "bogus", initial: 1 }); errors.push(false); } catch (e) { errors.push(e instanceof TypeError); }
  try { new WebAssembly.Table({ element: "funcref" }); errors.push(false); } catch (e) { errors.push(e instanceof TypeError); }
  try { new WebAssembly.Table({ element: "funcref", initial: 2, maximum: 1 }); errors.push(false); } catch (e) { errors.push(e instanceof RangeError); }
  const t = new WebAssembly.Table({ element: "externref", initial: 2 });
  const errs = [];
  try { t.get(0); errs.push(false); } catch (e) { errs.push(e instanceof TypeError); }
  try { t.set(0, null); errs.push(false); } catch (e) { errs.push(e instanceof TypeError); }
  try { t.grow(1); errs.push(false); } catch (e) { errs.push(e instanceof TypeError); }
  return [errors, t.length, errs];
}
export function receivers() {
  const out = [];
  const o = {};
  try { WebAssembly.Memory.prototype.grow.call(o, 0); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  try { WebAssembly.Global.prototype.valueOf.call(o); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  try { WebAssembly.Table.prototype.get.call(o, 0); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  return out;
}
export function errors() {
  const out = [];
  const ce = new WebAssembly.CompileError("a");
  const le = new WebAssembly.LinkError();
  const re = new WebAssembly.RuntimeError("b");
  out.push(ce instanceof WebAssembly.CompileError, ce.name, ce.message, le.name, le.message, re.message);
  try { new WebAssembly.CompileError(Symbol("x")); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  return out;
}`)
	ctx := context.Background()
	if got, err := ex.Call(ctx, "globals"); err != nil {
		t.Fatalf("globals: %v", err)
	} else {
		wantJSON(t, got, `["7",1.5,2.5,[true,true,true]]`)
	}
	if got, err := ex.Call(ctx, "memories"); err != nil {
		t.Fatalf("memories: %v", err)
	} else {
		wantJSON(t, got, `[[true,true,true,true,true],-1,1,65536]`)
	}
	if got, err := ex.Call(ctx, "tables"); err != nil {
		t.Fatalf("tables: %v", err)
	} else {
		wantJSON(t, got, `[[true,true,true,true],2,[true,true,true]]`)
	}
	if got, err := ex.Call(ctx, "receivers"); err != nil {
		t.Fatalf("receivers: %v", err)
	} else {
		wantJSON(t, got, `[true,true,true]`)
	}
	if got, err := ex.Call(ctx, "errors"); err != nil {
		t.Fatalf("errors: %v", err)
	} else {
		wantJSON(t, got, `[true,"CompileError","a","LinkError","","b",true]`)
	}
}

func TestCoverageWasmImportErrors(t *testing.T) {
	ex := wasmTestExec(t, `
export async function kind(bytes, imports) {
  try { await WebAssembly.instantiate(bytes, imports); return "no"; }
  catch (e) { return e.constructor.name; }
}
export function introspection(bytes) {
  const out = [];
  try { WebAssembly.Module.exports(1); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  try { WebAssembly.Module.imports(1); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  try { WebAssembly.Module.customSections(1, "x"); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  out.push(WebAssembly.Module.customSections(new WebAssembly.Module(bytes), "missing").length);
  return out;
}
export function badCalls(bytes) {
  const out = [];
  for (const f of ["compile", "instantiate", "validate"]) {
    try { WebAssembly[f](1); out.push(false); } catch (e) { out.push(e instanceof TypeError || e instanceof WebAssembly.CompileError); }
  }
  try { new WebAssembly.Module(1); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  try { new WebAssembly.Instance(1); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  return out;
}`)
	ctx := context.Background()
	cases := []struct {
		bytes   []byte
		imports string
	}{
		{testImportFuncModule(), `{ env: 1 }`},
		{testImportFuncModule(), `{ env: { double: 1 } }`},
		{testImportMemoryModule(), `{ env: { memory: 1 } }`},
		{testImportGlobalModule(), `{ env: { g: 1 } }`},
		{testImportFuncModule(), `{}`},
	}
	for i, c := range cases {
		src := fmt.Sprintf(`export async function kind(bytes, imports) {
  try { await WebAssembly.instantiate(bytes, imports); return "no"; }
  catch (e) { return e.constructor.name; }
}
export async function run(bytes) { return kind(bytes, %s); }`, c.imports)
		e := wasmTestExec(t, src)
		got, err := e.Call(ctx, "run", c.bytes)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got != "LinkError" {
			t.Fatalf("case %d kind = %#v", i, got)
		}
	}
	if got, err := ex.Call(ctx, "introspection", testAddModule()); err != nil {
		t.Fatalf("introspection: %v", err)
	} else {
		wantJSON(t, got, `[true,true,true,0]`)
	}
	if got, err := ex.Call(ctx, "badCalls", testAddModule()); err != nil {
		t.Fatalf("badCalls: %v", err)
	} else {
		wantJSON(t, got, `[true,true,true,true,true]`)
	}
}

func TestCoverageWasmUnsupportedImports(t *testing.T) {
	makeImports := func(kind wasmKind) []byte {
		return testModule(testTypeSection(wasmFuncSig{}), func() []byte {
			p := appendU32(nil, 1)
			p = appendString(p, "env")
			p = appendString(p, "x")
			p = append(p, byte(kind))
			switch kind {
			case wasmKindTable:
				p = append(p, byte(wasmValueTypeFuncref))
				p = appendLimits(p, wasmLimits{min: 1})
			case wasmKindMemory:
				p = appendLimits(p, wasmLimits{min: 1})
			case wasmKindTag:
				p = append(p, 0)
				p = appendU32(p, 0)
			}
			return wasmSection(2, p)
		}())
	}
	ex := wasmTestExec(t, `
export async function kind(bytes, imports) {
  try { await WebAssembly.instantiate(bytes, imports); return "no"; }
  catch (e) { return e.constructor.name; }
}`)
	ctx := context.Background()
	if got, err := ex.Call(ctx, "kind", makeImports(wasmKindTable), `{ env: {} }`); err != nil || got != "LinkError" {
		t.Fatalf("table import = %v, %v", got, err)
	}
	// wazero rejects tag imports before linking because exception handling is
	// not enabled; either error class is acceptable here.
	if got, err := ex.Call(ctx, "kind", makeImports(wasmKindTag), `{ env: {} }`); err != nil || (got != "LinkError" && got != "CompileError") {
		t.Fatalf("tag import = %v, %v", got, err)
	}
	// Two memory imports in one namespace are rejected.
	twoMemories := testModule(func() []byte {
		p := appendU32(nil, 2)
		for _, name := range []string{"a", "b"} {
			p = appendString(p, "env")
			p = appendString(p, name)
			p = append(p, byte(wasmKindMemory))
			p = appendLimits(p, wasmLimits{min: 1})
		}
		return wasmSection(2, p)
	}())
	if got, err := ex.Call(ctx, "kind", twoMemories, `{ env: { a: new WebAssembly.Memory({initial:1}), b: new WebAssembly.Memory({initial:1}) } }`); err != nil || got != "LinkError" {
		t.Fatalf("two memories = %v, %v", got, err)
	}
}

func TestCoverageWasmMultiValueImport(t *testing.T) {
	ex := wasmTestExec(t, `
export async function run(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, {
    env: { pair: () => [4, 5] },
  });
  return instance.exports.pair();
}`)
	mod := testModule(
		testTypeSection(
			wasmFuncSig{results: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}},
			wasmFuncSig{results: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}},
		),
		testImportSection(wasmImportSpec{module: "env", name: "pair", kind: wasmKindFunc, typeIndex: 0}),
		testFunctionSection(1),
		testExportSection(wasmExportSpec{name: "pair", kind: wasmKindFunc, index: 1}),
		testCodeSection([]byte{0x00, 0x10, 0x00, 0x0b}),
	)
	got, err := ex.Call(context.Background(), "run", mod)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantJSON(t, got, `[4,5]`)

	bad := wasmTestExec(t, `
export async function run(bytes) {
  const { instance } = await WebAssembly.instantiate(bytes, { env: { pair: () => 1 } });
  try { instance.exports.pair(); return "no"; } catch (e) { return e.constructor.name; }
}`)
	got, err = bad.Call(context.Background(), "run", mod)
	if err != nil {
		t.Fatalf("bad run: %v", err)
	}
	if got != "TypeError" {
		t.Fatalf("bad pair = %#v", got)
	}
}

func TestCoverageWasmWASIConfigBranches(t *testing.T) {
	var stdout, stderr strings.Builder
	got, err := New(Config{
		Name:   "wasi-branches.js",
		Source: `export async function run(bytes) { const { instance } = await WebAssembly.instantiate(bytes, {}); instance.exports._start(); return "ok"; }`,
		WebAssembly: WasmOptions{WASI: WasiOptions{
			Args:   []string{"app"},
			Env:    map[string]string{"A": "1"},
			FS:     fstest.MapFS{},
			Stdin:  strings.NewReader(""),
			Stdout: &stdout,
			Stderr: &stderr,
			Rand:   strings.NewReader(strings.Repeat("x", 64)),
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer got.Close()
	if _, err := got.Call(context.Background(), "run", testWASIModule()); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestCoverageWasmJSTrapUnwrap(t *testing.T) {
	trap := &wasmJSTrap{err: io.EOF}
	if trap.Error() != "EOF" || trap.Unwrap() != io.EOF {
		t.Fatalf("wasmJSTrap = %q, %v", trap.Error(), trap.Unwrap())
	}
}

func TestCoverageWasmDescriptorEdges(t *testing.T) {
	ex := wasmTestExec(t, `
export function globals() {
  const g = new WebAssembly.Global({ value: "i32" });
  const out = [g.value];
  try { new WebAssembly.Global({ value: "i64" }, 1); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  try { new WebAssembly.Global({ value: Symbol("x") }, 1); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  try { new WebAssembly.Global({ get value() { throw new TypeError("getter"); } }, 1); out.push(false); } catch (e) { out.push(e instanceof TypeError); }
  return out;
}
export function memories() {
  const m = new WebAssembly.Memory({ initial: 1, maximum: null });
  const extra = [];
  try { new WebAssembly.Memory({ initial: Symbol("x") }); extra.push(false); } catch (e) { extra.push(e instanceof TypeError); }
  try { new WebAssembly.Memory({ initial: 1, maximum: Symbol("x") }); extra.push(false); } catch (e) { extra.push(e instanceof TypeError); }
  return [m.buffer.byteLength, m.grow(0), extra];
}
export function tables() {
  const t = new WebAssembly.Table({ element: "externref", initial: 1, maximum: null });
  try { new WebAssembly.Table({ element: "funcref", initial: 1, maximum: Symbol("x") }); return "no"; } catch (e) { return [t.length, e instanceof TypeError]; }
}
export function compileBad(bytes) {
  return WebAssembly.compile(bytes).then(function () { return "ok"; }, function (e) { return e.constructor.name; });
}
export function memoryReceiver() {
  try { Object.getOwnPropertyDescriptor(WebAssembly.Memory.prototype, "buffer").get.call({}); return false; }
  catch (e) { return e instanceof TypeError; }
}
export function tableReceiver() {
  try { Object.getOwnPropertyDescriptor(WebAssembly.Table.prototype, "length").get.call({}); return false; }
  catch (e) { return e instanceof TypeError; }
}
export function subclass() {
  function C() {}
  C.prototype = WebAssembly.Global.prototype;
  const g = Reflect.construct(WebAssembly.Global, [{ value: "i32" }, 1], C);
  return [g instanceof C, g instanceof WebAssembly.Global, g.value];
}`)
	ctx := context.Background()
	if got, err := ex.Call(ctx, "globals"); err != nil {
		t.Fatalf("globals: %v", err)
	} else {
		wantJSON(t, got, `[0,true,true,true]`)
	}
	if got, err := ex.Call(ctx, "memories"); err != nil {
		t.Fatalf("memories: %v", err)
	} else {
		wantJSON(t, got, `[65536,1,[true,true]]`)
	}
	if got, err := ex.Call(ctx, "tables"); err != nil {
		t.Fatalf("tables: %v", err)
	} else {
		wantJSON(t, got, `[1,true]`)
	}
	if got, err := ex.Call(ctx, "compileBad", []byte{0, 1, 2}); err != nil || got != "CompileError" {
		t.Fatalf("compileBad = %#v, %v", got, err)
	}
	if got, err := ex.Call(ctx, "memoryReceiver"); err != nil || got != true {
		t.Fatalf("memoryReceiver = %#v, %v", got, err)
	}
	if got, err := ex.Call(ctx, "tableReceiver"); err != nil || got != true {
		t.Fatalf("tableReceiver = %#v, %v", got, err)
	}
	if got, err := ex.Call(ctx, "subclass"); err != nil {
		t.Fatalf("subclass: %v", err)
	} else {
		wantJSON(t, got, `[true,true,1]`)
	}
}

func TestCoverageWasmDirectBranches(t *testing.T) {
	if _, err := wasmGlobalInit(api.ValueType(0x7b), 0); err == nil {
		t.Fatalf("wasmGlobalInit(v128) accepted")
	}
	rt, err := NewVM(Config{Source: "1"})
	if err != nil {
		t.Fatalf("NewVM: %v", err)
	}
	defer rt.Close()
	s := &wasmState{}
	if _, err := s.fromWasm(rt.raw.Realm(), api.ValueType(0x7b), 0); err == nil {
		t.Fatalf("fromWasm(v128) accepted")
	}
	if _, err := s.fromWasmResults(rt.raw.Realm(), []api.ValueType{api.ValueType(0x7b)}, []uint64{0}); err == nil {
		t.Fatalf("fromWasmResults(v128) accepted")
	}
	if b := appendSLEB(nil, 1<<40); len(b) < 5 {
		t.Fatalf("appendSLEB large = %v", b)
	}
	if b := appendSLEB(nil, -1<<40); len(b) < 5 {
		t.Fatalf("appendSLEB large negative = %v", b)
	}
}

func TestCoverageWasmInstantiateSupport(t *testing.T) {
	ctx := context.Background()
	wr := wazero.NewRuntime(ctx)
	defer wr.Close(ctx)
	s := &wasmState{wr: wr}
	if _, err := s.instantiateSupport(ctx, "bad", []byte{0, 1, 2}); err == nil {
		t.Fatalf("invalid support module accepted")
	}
	bin := (&wasmModuleBuilder{
		memories: []wasmLimits{{min: 1}},
		exports:  []wasmExportSpec{{name: "memory", kind: wasmKindMemory}},
	}).build()
	if _, err := s.instantiateSupport(ctx, "dup", bin); err != nil {
		t.Fatalf("support module: %v", err)
	}
	if _, err := s.instantiateSupport(ctx, "dup", bin); err == nil {
		t.Fatalf("duplicate support module name accepted")
	}
}

func TestCoverageWasmToWasmUnsupported(t *testing.T) {
	rt, err := NewVM(Config{Source: "1"})
	if err != nil {
		t.Fatalf("NewVM: %v", err)
	}
	defer rt.Close()
	s := &wasmState{}
	if _, err := s.toWasm(rt.raw.Realm(), api.ValueType(0x7b), moejs.Number(1)); err == nil {
		t.Fatalf("toWasm(v128) accepted")
	}
}
