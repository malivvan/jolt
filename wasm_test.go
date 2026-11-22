//go:build canopy_wasm && !(js && wasm)

package js

import (
	"context"
	"encoding/json"
	"testing"

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
		WebAssembly: WebAssemblyOptions{Disable: true},
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
