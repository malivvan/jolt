//go:build !nowasm && !(js && wasm)

package js

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// wasmValueTypeFuncref is the funcref value type. wazero's api package does
// not export a constant for it.
const wasmValueTypeFuncref api.ValueType = 0x70

// wasmMemory is the state behind a WebAssembly.Memory object. supportModule
// and supportExport name a module in the wazero runtime that exports mem, so
// another module can import it through a synthetic namespace module.
type wasmMemory struct {
	state         *wasmState
	mem           api.Memory
	supportModule string
	supportExport string
	limits        wasmLimits
	buf           *engine.Object
	bufSize       uint32
}

// wasmGlobal is the state behind a WebAssembly.Global object.
type wasmGlobal struct {
	state         *wasmState
	g             api.Global
	supportModule string
	supportExport string
	valueType     api.ValueType
	mutable       bool
}

// wasmTable is the state behind a WebAssembly.Table object. wazero's public
// API does not expose tables, so Table is present for API shape and reports
// an error on element access.
type wasmTable struct {
	state       *wasmState
	elementType api.ValueType
	length      uint32
	max         uint32
	hasMax      bool
}

// wasmJSTrap carries a JS exception through a wazero host function panic.
// Wazero recovers panics and returns an error that errors.As can inspect.
type wasmJSTrap struct {
	err error
}

func (e *wasmJSTrap) Error() string { return e.err.Error() }
func (e *wasmJSTrap) Unwrap() error { return e.err }

func (s *wasmState) ctorMemory(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
	mem, err := s.parseMemoryDescriptor(moejs.Arg(args, 0))
	if err != nil {
		return engine.Undefined(), err
	}
	o, err := s.newObject(s.memoryClass, newTarget, mem)
	if err != nil {
		return engine.Undefined(), err
	}
	return engine.ObjectValue(o), nil
}

func (s *wasmState) parseMemoryDescriptor(desc engine.Value) (*wasmMemory, error) {
	r := s.realm
	if !desc.IsObject() {
		return nil, r.TypeError("new WebAssembly.Memory(): argument 0 must be a descriptor object")
	}
	initialVal, err := s.rt.raw.Get(desc, "initial")
	if err != nil {
		return nil, err
	}
	if initialVal.IsUndefined() {
		return nil, r.TypeError("new WebAssembly.Memory(): descriptor.initial is required")
	}
	initial, err := r.ToUint32(initialVal)
	if err != nil {
		return nil, err
	}
	if initial > s.memoryLimitPages {
		return nil, r.RangeError("new WebAssembly.Memory(): initial size %d exceeds the limit of %d pages", initial, s.memoryLimitPages)
	}
	sharedVal, err := s.rt.raw.Get(desc, "shared")
	if err != nil {
		return nil, err
	}
	if engine.ToBoolean(sharedVal) {
		return nil, r.TypeError("new WebAssembly.Memory(): shared memories are not supported by the wazero backend")
	}
	var maximum uint32
	var hasMax bool
	maxVal, err := s.rt.raw.Get(desc, "maximum")
	if err != nil {
		return nil, err
	}
	if !maxVal.IsUndefined() && !maxVal.IsNull() {
		maximum, err = r.ToUint32(maxVal)
		if err != nil {
			return nil, err
		}
		if maximum > s.memoryLimitPages {
			return nil, r.RangeError("new WebAssembly.Memory(): maximum size %d exceeds the limit of %d pages", maximum, s.memoryLimitPages)
		}
		if maximum < initial {
			return nil, r.RangeError("new WebAssembly.Memory(): maximum %d is less than initial %d", maximum, initial)
		}
		hasMax = true
	}
	return s.newMemory(initial, maximum, hasMax, false)
}

func (s *wasmState) newMemory(min, max uint32, hasMax, shared bool) (*wasmMemory, error) {
	if shared {
		return nil, s.realm.TypeError("shared WebAssembly.Memory is not supported by the wazero backend")
	}
	name := s.nextName("memory")
	b := &wasmModuleBuilder{}
	b.memories = append(b.memories, wasmLimits{min: min, max: max, hasMax: hasMax})
	b.exports = append(b.exports, wasmExportSpec{name: "memory", kind: wasmKindMemory, index: 0})
	mod, err := s.instantiateSupport(context.Background(), name, b.build())
	if err != nil {
		return nil, err
	}
	mem := mod.ExportedMemory("memory")
	if mem == nil {
		return nil, fmt.Errorf("js: internal WebAssembly memory support module has no memory export")
	}
	return &wasmMemory{
		state:         s,
		mem:           mem,
		supportModule: name,
		supportExport: "memory",
		limits:        wasmLimits{min: min, max: max, hasMax: hasMax},
	}, nil
}

func (s *wasmState) instantiateSupport(ctx context.Context, name string, binary []byte) (api.Module, error) {
	cm, err := s.wr.CompileModule(ctx, binary)
	if err != nil {
		return nil, fmt.Errorf("js: internal WebAssembly support module: %w", err)
	}
	mod, err := s.wr.InstantiateModule(ctx, cm, wazero.NewModuleConfig().WithName(name).WithStartFunctions())
	if err != nil {
		_ = cm.Close(ctx)
		return nil, fmt.Errorf("js: internal WebAssembly support module: %w", err)
	}
	return mod, nil
}

func (s *wasmState) memoryBuffer(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
	m, err := wasmThis[*wasmMemory](r, this, "get buffer")
	if err != nil {
		return engine.Undefined(), err
	}
	return m.bufferValue(r)
}

func (m *wasmMemory) bufferValue(r *engine.Realm) (engine.Value, error) {
	pages, ok := m.mem.Grow(0)
	if !ok {
		return engine.Undefined(), m.state.throwError(m.state.runtimeErrClass, "WebAssembly.Memory is detached")
	}
	size := uint64(pages) << 16
	if size > math.MaxInt32-1 {
		return engine.Undefined(), r.RangeError("WebAssembly.Memory.buffer: %d bytes exceeds the maximum ArrayBuffer length", size)
	}
	if m.buf != nil && m.bufSize == uint32(size) {
		return engine.ObjectValue(m.buf), nil
	}
	var data []byte
	if size > 0 {
		data, ok = m.mem.Read(0, uint32(size))
		if !ok {
			return engine.Undefined(), r.RangeError("WebAssembly.Memory.buffer: memory is out of range")
		}
	}
	buf, err := r.NewArrayBuffer(data)
	if err != nil {
		return engine.Undefined(), err
	}
	m.buf = buf
	m.bufSize = uint32(size)
	return engine.ObjectValue(buf), nil
}

func (s *wasmState) memoryGrow(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	m, err := wasmThis[*wasmMemory](r, this, "WebAssembly.Memory.grow")
	if err != nil {
		return engine.Undefined(), err
	}
	delta, err := r.ToUint32(moejs.Arg(args, 0))
	if err != nil {
		return engine.Undefined(), err
	}
	prev, ok := m.mem.Grow(delta)
	if !ok {
		// The JS API reports a failed growth by returning -1; only a
		// non-convertible argument is a RangeError.
		return moejs.Number(-1), nil
	}
	m.buf = nil
	m.bufSize = 0
	return moejs.Number(float64(prev)), nil
}

func (s *wasmState) ctorGlobal(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
	g, err := s.parseGlobalDescriptor(moejs.Arg(args, 0), moejs.Arg(args, 1))
	if err != nil {
		return engine.Undefined(), err
	}
	o, err := s.newObject(s.globalClass, newTarget, g)
	if err != nil {
		return engine.Undefined(), err
	}
	return engine.ObjectValue(o), nil
}

func (s *wasmState) parseGlobalDescriptor(desc, initial engine.Value) (*wasmGlobal, error) {
	r := s.realm
	if !desc.IsObject() {
		return nil, r.TypeError("new WebAssembly.Global(): argument 0 must be a descriptor object")
	}
	typeVal, err := s.rt.raw.Get(desc, "value")
	if err != nil {
		return nil, err
	}
	typeStr, err := r.ToString(typeVal)
	if err != nil {
		return nil, err
	}
	valueType, ok := globalValueType(typeStr.GoString())
	if !ok {
		return nil, r.TypeError("new WebAssembly.Global(): unsupported value type %q", typeStr.GoString())
	}
	mutableVal, err := s.rt.raw.Get(desc, "mutable")
	if err != nil {
		return nil, err
	}
	mutable := engine.ToBoolean(mutableVal)
	if initial.IsUndefined() {
		if valueType == api.ValueTypeI64 {
			initial = moejs.Int(0)
		} else {
			initial = moejs.Number(0)
		}
	}
	bits, err := s.toWasm(r, valueType, initial)
	if err != nil {
		return nil, err
	}
	return s.newGlobal(valueType, mutable, bits)
}

func globalValueType(name string) (api.ValueType, bool) {
	switch name {
	case "i32":
		return api.ValueTypeI32, true
	case "i64":
		return api.ValueTypeI64, true
	case "f32":
		return api.ValueTypeF32, true
	case "f64":
		return api.ValueTypeF64, true
	default:
		return 0, false
	}
}

func (s *wasmState) newGlobal(valueType api.ValueType, mutable bool, bits uint64) (*wasmGlobal, error) {
	init, err := wasmGlobalInit(valueType, bits)
	if err != nil {
		return nil, s.realm.TypeError("%s", err.Error())
	}
	name := s.nextName("global")
	b := &wasmModuleBuilder{}
	b.globals = append(b.globals, wasmGlobalSpec{valueType: valueType, mutable: mutable, init: init})
	b.exports = append(b.exports, wasmExportSpec{name: "global", kind: wasmKindGlobal, index: 0})
	mod, err := s.instantiateSupport(context.Background(), name, b.build())
	if err != nil {
		return nil, err
	}
	g := mod.ExportedGlobal("global")
	if g == nil {
		return nil, fmt.Errorf("js: internal WebAssembly global support module has no global export")
	}
	return &wasmGlobal{
		state:         s,
		g:             g,
		supportModule: name,
		supportExport: "global",
		valueType:     valueType,
		mutable:       mutable,
	}, nil
}

func wasmGlobalInit(valueType api.ValueType, bits uint64) ([]byte, error) {
	switch valueType {
	case api.ValueTypeI32:
		return wasmConstI32(int32(uint32(bits))), nil
	case api.ValueTypeI64:
		return wasmConstI64(int64(bits)), nil
	case api.ValueTypeF32:
		return wasmConstF32(math.Float32frombits(uint32(bits))), nil
	case api.ValueTypeF64:
		return wasmConstF64(math.Float64frombits(bits)), nil
	default:
		return nil, fmt.Errorf("unsupported WebAssembly global type %s", api.ValueTypeName(valueType))
	}
}

func (s *wasmState) globalGet(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
	g, err := wasmThis[*wasmGlobal](r, this, "get value")
	if err != nil {
		return engine.Undefined(), err
	}
	return s.fromWasm(r, g.valueType, g.g.Get())
}

func (s *wasmState) globalSet(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	g, err := wasmThis[*wasmGlobal](r, this, "set value")
	if err != nil {
		return engine.Undefined(), err
	}
	if !g.mutable {
		return engine.Undefined(), r.TypeError("WebAssembly.Global is immutable")
	}
	mg, ok := g.g.(api.MutableGlobal)
	if !ok {
		return engine.Undefined(), r.TypeError("WebAssembly.Global is immutable")
	}
	bits, err := s.toWasm(r, g.valueType, moejs.Arg(args, 0))
	if err != nil {
		return engine.Undefined(), err
	}
	mg.Set(bits)
	return engine.Undefined(), nil
}

func (s *wasmState) parseTableDescriptor(desc engine.Value) (*wasmTable, error) {
	r := s.realm
	if !desc.IsObject() {
		return nil, r.TypeError("new WebAssembly.Table(): argument 0 must be a descriptor object")
	}
	elemVal, err := s.rt.raw.Get(desc, "element")
	if err != nil {
		return nil, err
	}
	elemStr, err := r.ToString(elemVal)
	if err != nil {
		return nil, err
	}
	var elem api.ValueType
	switch elemStr.GoString() {
	case "anyfunc", "funcref":
		elem = wasmValueTypeFuncref
	case "externref":
		elem = api.ValueTypeExternref
	default:
		return nil, r.TypeError("new WebAssembly.Table(): unsupported element type %q", elemStr.GoString())
	}
	initialVal, err := s.rt.raw.Get(desc, "initial")
	if err != nil {
		return nil, err
	}
	if initialVal.IsUndefined() {
		return nil, r.TypeError("new WebAssembly.Table(): descriptor.initial is required")
	}
	initial, err := r.ToUint32(initialVal)
	if err != nil {
		return nil, err
	}
	t := &wasmTable{state: s, elementType: elem, length: initial}
	maxVal, err := s.rt.raw.Get(desc, "maximum")
	if err != nil {
		return nil, err
	}
	if !maxVal.IsUndefined() && !maxVal.IsNull() {
		max, err := r.ToUint32(maxVal)
		if err != nil {
			return nil, err
		}
		if max < initial {
			return nil, r.RangeError("new WebAssembly.Table(): maximum %d is less than initial %d", max, initial)
		}
		t.max = max
		t.hasMax = true
	}
	return t, nil
}

func (s *wasmState) tableLength(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
	t, err := wasmThis[*wasmTable](r, this, "get length")
	if err != nil {
		return engine.Undefined(), err
	}
	return moejs.Number(float64(t.length)), nil
}

func (s *wasmState) tableGet(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	return engine.Undefined(), r.TypeError("WebAssembly.Table.get(): tables are not supported by the wazero backend")
}

func (s *wasmState) tableSet(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	return engine.Undefined(), r.TypeError("WebAssembly.Table.set(): tables are not supported by the wazero backend")
}

func (s *wasmState) tableGrow(r *engine.Realm, this engine.Value, args []engine.Value) (engine.Value, error) {
	return engine.Undefined(), r.TypeError("WebAssembly.Table.grow(): tables are not supported by the wazero backend")
}

func (s *wasmState) newMemoryObject(m *wasmMemory) (engine.Value, error) {
	o, err := s.newObject(s.memoryClass, nil, m)
	if err != nil {
		return engine.Undefined(), err
	}
	return engine.ObjectValue(o), nil
}

func (s *wasmState) newGlobalObject(g *wasmGlobal) (engine.Value, error) {
	o, err := s.newObject(s.globalClass, nil, g)
	if err != nil {
		return engine.Undefined(), err
	}
	return engine.ObjectValue(o), nil
}

func (s *wasmState) newTableObject(t *wasmTable) (engine.Value, error) {
	o, err := s.newObject(s.tableClass, nil, t)
	if err != nil {
		return engine.Undefined(), err
	}
	return engine.ObjectValue(o), nil
}

func (s *wasmState) buildExports(inst *wasmInstance) *engine.Object {
	r := s.realm
	exports := r.NewObject()
	info := inst.module.info
	for _, e := range info.exports {
		var v engine.Value
		switch e.kind {
		case wasmKindFunc:
			fn := inst.api.ExportedFunction(e.name)
			if fn == nil {
				continue
			}
			v = engine.ObjectValue(s.exportFunc(e.name, fn))
		case wasmKindMemory:
			mem := inst.api.ExportedMemory(e.name)
			if mem == nil {
				continue
			}
			def := mem.Definition()
			max, hasMax := def.Max()
			wm := &wasmMemory{
				state:         s,
				mem:           mem,
				supportModule: inst.name,
				supportExport: e.name,
				limits:        wasmLimits{min: def.Min(), max: max, hasMax: hasMax},
			}
			var err error
			if v, err = s.newMemoryObject(wm); err != nil {
				continue
			}
		case wasmKindGlobal:
			g := inst.api.ExportedGlobal(e.name)
			if g == nil {
				continue
			}
			_, mutable := g.(api.MutableGlobal)
			wg := &wasmGlobal{
				state:         s,
				g:             g,
				supportModule: inst.name,
				supportExport: e.name,
				valueType:     g.Type(),
				mutable:       mutable,
			}
			var err error
			if v, err = s.newGlobalObject(wg); err != nil {
				continue
			}
		case wasmKindTable:
			ti := wasmTableInfo{}
			if int(e.index) < len(info.tables) {
				ti = info.tables[e.index]
			}
			wt := &wasmTable{state: s, elementType: ti.elementType, length: ti.limits.min, max: ti.limits.max, hasMax: ti.limits.hasMax}
			var err error
			if v, err = s.newTableObject(wt); err != nil {
				continue
			}
		default:
			continue
		}
		exports.SetProp(r, r.KeyFromGoString(e.name), v)
	}
	return exports
}

func (s *wasmState) exportFunc(name string, fn api.Function) *engine.Object {
	def := fn.Definition()
	params := def.ParamTypes()
	results := def.ResultTypes()
	nf := func(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		in := make([]uint64, len(params))
		for i, t := range params {
			bits, err := s.toWasm(r, t, moejs.Arg(args, i))
			if err != nil {
				return engine.Undefined(), err
			}
			in[i] = bits
		}
		out, err := fn.Call(s.rt.currentContext(), in...)
		if err != nil {
			var trap *wasmJSTrap
			if errors.As(err, &trap) {
				return engine.Undefined(), trap.err
			}
			return engine.Undefined(), s.throwError(s.runtimeErrClass, err.Error())
		}
		return s.fromWasmResults(r, results, out)
	}
	return s.realm.NewNativeFunction(s.realm.InternGoString(name), len(params), nf)
}

func (s *wasmState) importFunc(js engine.Value, params, results []api.ValueType) api.GoModuleFunc {
	return func(ctx context.Context, _ api.Module, stack []uint64) {
		r := s.realm
		args := make([]engine.Value, len(params))
		for i, t := range params {
			v, err := s.fromWasm(r, t, stack[i])
			if err != nil {
				panic(&wasmJSTrap{err: err})
			}
			args[i] = v
		}
		res, err := s.rt.raw.CallFunction(js, engine.Undefined(), args...)
		if err != nil {
			panic(&wasmJSTrap{err: err})
		}
		switch len(results) {
		case 0:
			return
		case 1:
			bits, err := s.toWasm(r, results[0], res)
			if err != nil {
				panic(&wasmJSTrap{err: err})
			}
			stack[0] = bits
			return
		default:
			if !res.IsObject() {
				panic(&wasmJSTrap{err: r.TypeError("WebAssembly multi-value import must return an array")})
			}
			for i, t := range results {
				v, err := s.rt.raw.Get(res, strconv.Itoa(i))
				if err != nil {
					panic(&wasmJSTrap{err: err})
				}
				bits, err := s.toWasm(r, t, v)
				if err != nil {
					panic(&wasmJSTrap{err: err})
				}
				stack[i] = bits
			}
		}
	}
}

func (s *wasmState) toWasm(r *engine.Realm, t api.ValueType, v engine.Value) (uint64, error) {
	switch t {
	case api.ValueTypeI32:
		n, err := r.ToInt32(v)
		if err != nil {
			return 0, err
		}
		return api.EncodeI32(n), nil
	case api.ValueTypeI64:
		n, err := r.ToBigUint64(v)
		if err != nil {
			return 0, err
		}
		return n, nil
	case api.ValueTypeF32:
		n, err := r.ToNumber(v)
		if err != nil {
			return 0, err
		}
		return api.EncodeF32(float32(n)), nil
	case api.ValueTypeF64:
		n, err := r.ToNumber(v)
		if err != nil {
			return 0, err
		}
		return api.EncodeF64(n), nil
	default:
		return 0, r.TypeError("WebAssembly value type %s is not supported by the wazero backend", api.ValueTypeName(t))
	}
}

func (s *wasmState) fromWasm(r *engine.Realm, t api.ValueType, bits uint64) (engine.Value, error) {
	switch t {
	case api.ValueTypeI32:
		return moejs.Number(float64(api.DecodeI32(bits))), nil
	case api.ValueTypeI64:
		return engine.BigIntValue(engine.NewBigIntFromInt64(int64(bits))), nil
	case api.ValueTypeF32:
		return moejs.Number(float64(api.DecodeF32(bits))), nil
	case api.ValueTypeF64:
		return moejs.Number(api.DecodeF64(bits)), nil
	default:
		return engine.Undefined(), r.TypeError("WebAssembly value type %s is not supported by the wazero backend", api.ValueTypeName(t))
	}
}

func (s *wasmState) fromWasmResults(r *engine.Realm, results []api.ValueType, stack []uint64) (engine.Value, error) {
	switch len(results) {
	case 0:
		return engine.Undefined(), nil
	case 1:
		return s.fromWasm(r, results[0], stack[0])
	default:
		out := make([]engine.Value, len(results))
		for i, t := range results {
			v, err := s.fromWasm(r, t, stack[i])
			if err != nil {
				return engine.Undefined(), err
			}
			out[i] = v
		}
		return engine.ObjectValue(r.NewArrayFromSlice(out)), nil
	}
}
