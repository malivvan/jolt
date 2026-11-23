//go:build !nowasm && !(js && wasm)

// This file implements the browser WebAssembly JavaScript API on top of
// wazero. It is compiled by default; build with -tags nowasm (or for
// GOARCH=wasm) to leave it out.
package jolt

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// WebAssemblySupported reports whether this binary includes the WebAssembly
// JavaScript API. It is true unless jolt was built with the nowasm tag or
// for GOARCH=wasm.
const WebAssemblySupported = true

const (
	wasmPropW uint8 = 1 << 0 // writable
	wasmPropE uint8 = 1 << 1 // enumerable
	wasmPropC uint8 = 1 << 2 // configurable
)

// wasmState is one installation: it owns the wazero runtime and the JS
// constructors and prototypes of the WebAssembly global.
type wasmState struct {
	rt               *VM
	env              *Env
	wr               wazero.Runtime
	realm            *engine.Realm
	count            uint64
	memoryLimitPages uint32
	wasi             WasiOptions
	wasiEnabled      bool

	moduleClass   *wasmClass
	instanceClass *wasmClass
	memoryClass   *wasmClass
	globalClass   *wasmClass
	tableClass    *wasmClass

	compileErrClass *wasmClass
	linkErrClass    *wasmClass
	runtimeErrClass *wasmClass
}

type wasmClass struct {
	ctor  *engine.Object
	proto *engine.Object
	name  string
}

type wasmModule struct {
	state    *wasmState
	compiled wazero.CompiledModule
	info     *wasmBinaryInfo
}

type wasmInstance struct {
	state   *wasmState
	module  *wasmModule
	api     api.Module
	name    string
	exports *engine.Object
}

// installWebAssembly creates a browser-like wazero runtime and installs the
// WebAssembly global. The WASI host module is loaded unless the caller opts
// out; it does not run the conventional "_start" export unless the caller asks
// for it. The environment defaults to empty and sandboxed.
func installWebAssembly(env *Env, opts WasmOptions) (func() error, error) {
	if opts.Disable {
		return nil, nil
	}
	pages := opts.MemoryLimitPages
	if pages == 0 {
		pages = 65536 // 4 GiB, the browser limit
	}
	if pages > 65536 {
		return nil, fmt.Errorf("jolt: WebAssembly.MemoryLimitPages %d exceeds the 65536-page (4 GiB) limit", pages)
	}
	ctx := context.Background()
	cfg := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2).
		WithMemoryLimitPages(pages).
		WithMemoryCapacityFromMax(false).
		WithCloseOnContextDone(true).
		WithDebugInfoEnabled(false).
		WithCustomSections(true)
	wr := wazero.NewRuntimeWithConfig(ctx, cfg)
	s := &wasmState{
		rt: env.VM(), env: env, wr: wr, realm: env.Raw().Realm(),
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
	if err := s.install(ctx); err != nil {
		_ = wr.Close(ctx)
		return nil, err
	}
	return func() error { return wr.Close(context.Background()) }, nil
}

func (s *wasmState) install(ctx context.Context) error {
	s.moduleClass = s.newClass("Module", 1, s.ctorModule, nil)
	s.instanceClass = s.newClass("Instance", 2, s.ctorInstance, nil)
	s.memoryClass = s.newClass("Memory", 1, s.ctorMemory, nil)
	s.globalClass = s.newClass("Global", 1, s.ctorGlobal, nil)
	s.tableClass = s.newClass("Table", 1, s.ctorTable, nil)

	baseErr := s.realm.ErrorPrototypeFor(engine.KindError)
	s.compileErrClass = s.newErrorClass("CompileError", baseErr)
	s.linkErrClass = s.newErrorClass("LinkError", baseErr)
	s.runtimeErrClass = s.newErrorClass("RuntimeError", baseErr)

	s.installPrototypes()
	ns := s.newNamespace()
	if err := s.env.Set("WebAssembly", engine.ObjectValue(ns)); err != nil {
		return err
	}
	return s.installStreaming(ctx)
}

func (s *wasmState) newClass(name string, length int, ctor engine.NativeCtor, base *engine.Object) *wasmClass {
	r := s.realm
	c := r.NewNativeConstructor(r.InternGoString(name), length, nil, ctor)
	if base == nil {
		base = r.ObjectPrototype
	}
	proto := r.NewObjectWithProto(base)
	c.DefineOwnDataFast(r, engine.StringKey(r.InternGoString("prototype")), engine.ObjectValue(proto), wasmPropW|wasmPropC)
	proto.DefineOwnDataFast(r, engine.StringKey(r.InternGoString("constructor")), engine.ObjectValue(c), wasmPropW|wasmPropC)
	proto.DefineOwnDataFast(r, engine.SymbolKey(engine.SymToStringTag),
		engine.StringValue(r.InternGoString("WebAssembly."+name)), wasmPropC)
	return &wasmClass{ctor: c, proto: proto, name: name}
}

func (s *wasmState) newErrorClass(name string, base *engine.Object) *wasmClass {
	var cl *wasmClass
	ctor := func(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
		msg := ""
		if len(args) > 0 && !args[0].IsUndefined() {
			str, err := r.ToString(args[0])
			if err != nil {
				return engine.Undefined(), err
			}
			msg = str.GoString()
		}
		return s.newErrorValue(newTarget, cl, msg), nil
	}
	cl = s.newClass(name, 1, ctor, base)
	r := s.realm
	cl.proto.DefineOwnDataFast(r, r.KeyFromGoString("name"), engine.StringValue(r.InternGoString(name)), wasmPropW|wasmPropC)
	cl.proto.DefineOwnDataFast(r, r.KeyFromGoString("message"), engine.StringValue(r.InternGoString("")), wasmPropW|wasmPropC)
	return cl
}

func (s *wasmState) newErrorValue(newTarget *engine.Object, class *wasmClass, msg string) engine.Value {
	r := s.realm
	e := r.NewError(engine.KindError, "%s", msg)
	proto := class.proto
	if newTarget != nil {
		if p, err := r.GetPrototypeFromConstructor(newTarget, nil, class.proto); err == nil && p != nil {
			proto = p
		}
	}
	e.SetPrototypeOf(r, proto)
	return engine.ObjectValue(e)
}

func (s *wasmState) throwError(class *wasmClass, msg string) error {
	return &moejs.Exception{Value: s.newErrorValue(nil, class, msg)}
}

func (s *wasmState) installPrototypes() {
	s.accessor(s.instanceClass.proto, "exports", func(r *engine.Realm, this engine.Value, _ []engine.Value) (engine.Value, error) {
		inst, err := wasmThis[*wasmInstance](r, this, "get exports")
		if err != nil {
			return engine.Undefined(), err
		}
		if inst.exports == nil {
			return engine.Undefined(), nil
		}
		return engine.ObjectValue(inst.exports), nil
	}, nil)

	s.accessor(s.memoryClass.proto, "buffer", s.memoryBuffer, nil)
	s.method(s.memoryClass.proto, "grow", 1, s.memoryGrow)

	s.accessor(s.globalClass.proto, "value", s.globalGet, s.globalSet)
	s.method(s.globalClass.proto, "valueOf", 0, s.globalGet)

	s.accessor(s.tableClass.proto, "length", s.tableLength, nil)
	s.method(s.tableClass.proto, "get", 1, s.tableGet)
	s.method(s.tableClass.proto, "set", 2, s.tableSet)
	s.method(s.tableClass.proto, "grow", 1, s.tableGrow)
}

func (s *wasmState) newNamespace() *engine.Object {
	ns := s.realm.NewObject()
	s.defineValue(ns, "Module", engine.ObjectValue(s.moduleClass.ctor))
	s.defineValue(ns, "Instance", engine.ObjectValue(s.instanceClass.ctor))
	s.defineValue(ns, "Memory", engine.ObjectValue(s.memoryClass.ctor))
	s.defineValue(ns, "Global", engine.ObjectValue(s.globalClass.ctor))
	s.defineValue(ns, "Table", engine.ObjectValue(s.tableClass.ctor))
	s.defineValue(ns, "CompileError", engine.ObjectValue(s.compileErrClass.ctor))
	s.defineValue(ns, "LinkError", engine.ObjectValue(s.linkErrClass.ctor))
	s.defineValue(ns, "RuntimeError", engine.ObjectValue(s.runtimeErrClass.ctor))

	s.defineMethod(ns, "compile", 1, s.compileFunc)
	s.defineMethod(ns, "instantiate", 2, s.instantiateFunc)
	s.defineMethod(ns, "validate", 1, s.validateFunc)

	s.defineMethod(s.moduleClass.ctor, "exports", 1, s.moduleExportsFunc)
	s.defineMethod(s.moduleClass.ctor, "imports", 1, s.moduleImportsFunc)
	s.defineMethod(s.moduleClass.ctor, "customSections", 2, s.moduleCustomSectionsFunc)

	ns.DefineOwnDataFast(s.realm, engine.SymbolKey(engine.SymToStringTag),
		engine.StringValue(s.realm.InternGoString("WebAssembly")), wasmPropC)
	return ns
}

func (s *wasmState) defineValue(o *engine.Object, name string, v engine.Value) {
	o.DefineOwnDataFast(s.realm, s.realm.KeyFromGoString(name), v, wasmPropW|wasmPropC)
}

func (s *wasmState) defineMethod(o *engine.Object, name string, length int, fn engine.NativeFunc) {
	s.defineValue(o, name, engine.ObjectValue(s.realm.NewNativeFunction(s.realm.InternGoString(name), length, fn)))
}

func (s *wasmState) method(o *engine.Object, name string, length int, fn engine.NativeFunc) {
	o.DefineOwnDataFast(s.realm, s.realm.KeyFromGoString(name),
		engine.ObjectValue(s.realm.NewNativeFunction(s.realm.InternGoString(name), length, fn)), wasmPropW|wasmPropC)
}

func (s *wasmState) accessor(o *engine.Object, name string, get engine.NativeFunc, set engine.NativeFunc) {
	r := s.realm
	getFn := r.NewNativeFunction(r.InternGoString("get "+name), 0, get)
	var setFn *engine.Object
	if set != nil {
		setFn = r.NewNativeFunction(r.InternGoString("set "+name), 1, set)
	}
	o.DefineOwnAccessorFast(r, r.KeyFromGoString(name), getFn, setFn, wasmPropC)
}

func (s *wasmState) nextName(kind string) string {
	s.count++
	return fmt.Sprintf("jolt:wasm:%s:%d", kind, s.count)
}

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

func (s *wasmState) newObject(class *wasmClass, newTarget *engine.Object, state any) (*engine.Object, error) {
	r := s.realm
	var o *engine.Object
	if newTarget == nil || newTarget == class.ctor {
		o = r.NewObjectWithProto(class.proto)
	} else {
		var err error
		o, err = r.OrdinaryCreateFromConstructor(newTarget, class.proto, engine.ClassObject)
		if err != nil {
			return nil, err
		}
	}
	o.SetInternal(state)
	return o, nil
}

func wasmThis[T any](r *engine.Realm, this engine.Value, method string) (T, error) {
	if this.IsObject() {
		if v, ok := this.AsObject().Internal().(T); ok {
			return v, nil
		}
	}
	var zero T
	return zero, r.TypeError("Method %s called on incompatible receiver", method)
}

func (s *wasmState) moduleFromValue(v engine.Value) (*wasmModule, bool) {
	if !v.IsObject() {
		return nil, false
	}
	m, ok := v.AsObject().Internal().(*wasmModule)
	return m, ok
}

func (s *wasmState) memoryFromValue(v engine.Value) (*wasmMemory, bool) {
	if !v.IsObject() {
		return nil, false
	}
	m, ok := v.AsObject().Internal().(*wasmMemory)
	return m, ok
}

func (s *wasmState) globalFromValue(v engine.Value) (*wasmGlobal, bool) {
	if !v.IsObject() {
		return nil, false
	}
	g, ok := v.AsObject().Internal().(*wasmGlobal)
	return g, ok
}

func (s *wasmState) bufferSource(v engine.Value, call string) ([]byte, error) {
	if v.IsUndefined() || v.IsNull() {
		return nil, s.realm.TypeError("%s: argument 0 must be a BufferSource", call)
	}
	g, err := s.rt.raw.ToGo(v)
	if err != nil {
		return nil, s.realm.TypeError("%s: argument 0 must be a BufferSource", call)
	}
	if b, ok := g.([]byte); ok {
		return b, nil
	}
	return nil, s.realm.TypeError("%s: argument 0 must be a BufferSource", call)
}

func (s *wasmState) compile(ctx context.Context, binary []byte) (*wasmModule, error) {
	cm, err := s.wr.CompileModule(ctx, binary)
	if err != nil {
		return nil, s.throwError(s.compileErrClass, err.Error())
	}
	info, err := parseWasmBinary(binary)
	if err != nil {
		_ = cm.Close(ctx)
		return nil, s.throwError(s.compileErrClass, err.Error())
	}
	return &wasmModule{state: s, compiled: cm, info: info}, nil
}

func (s *wasmState) ctorModule(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
	b, err := s.bufferSource(moejs.Arg(args, 0), "new WebAssembly.Module()")
	if err != nil {
		return engine.Undefined(), err
	}
	m, err := s.compile(s.rt.currentContext(), b)
	if err != nil {
		return engine.Undefined(), err
	}
	o, err := s.newObject(s.moduleClass, newTarget, m)
	if err != nil {
		return engine.Undefined(), err
	}
	return engine.ObjectValue(o), nil
}

func (s *wasmState) ctorInstance(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
	m, ok := s.moduleFromValue(moejs.Arg(args, 0))
	if !ok {
		return engine.Undefined(), r.TypeError("WebAssembly.Instance(): argument 0 must be a WebAssembly.Module")
	}
	inst, err := s.instantiate(s.rt.currentContext(), m, moejs.Arg(args, 1))
	if err != nil {
		return engine.Undefined(), err
	}
	o, err := s.newObject(s.instanceClass, newTarget, inst)
	if err != nil {
		return engine.Undefined(), err
	}
	return engine.ObjectValue(o), nil
}

func (s *wasmState) ctorTable(r *engine.Realm, args []engine.Value, newTarget *engine.Object) (engine.Value, error) {
	t, err := s.parseTableDescriptor(moejs.Arg(args, 0))
	if err != nil {
		return engine.Undefined(), err
	}
	o, err := s.newObject(s.tableClass, newTarget, t)
	if err != nil {
		return engine.Undefined(), err
	}
	return engine.ObjectValue(o), nil
}

// promise settles a host promise with compute's result. It must be called on
// the event loop, as host functions are.
func (s *wasmState) promise(compute func() (engine.Value, error)) engine.Value {
	p, settle := s.env.Loop().NewPromise()
	settle(func(*moejs.Runtime) (moejs.Value, error) {
		return compute()
	})
	return p
}

func (s *wasmState) compileFunc(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	b, err := s.bufferSource(moejs.Arg(args, 0), "WebAssembly.compile()")
	if err != nil {
		return engine.Undefined(), err
	}
	m, err := s.compile(s.rt.currentContext(), b)
	return s.promise(func() (engine.Value, error) {
		if err != nil {
			return engine.Undefined(), err
		}
		o, err := s.newObject(s.moduleClass, nil, m)
		if err != nil {
			return engine.Undefined(), err
		}
		return engine.ObjectValue(o), nil
	}), nil
}

func (s *wasmState) instantiateFunc(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	ctx := s.rt.currentContext()
	if m, ok := s.moduleFromValue(moejs.Arg(args, 0)); ok {
		inst, err := s.instantiate(ctx, m, moejs.Arg(args, 1))
		return s.promise(func() (engine.Value, error) {
			if err != nil {
				return engine.Undefined(), err
			}
			o, err := s.newObject(s.instanceClass, nil, inst)
			if err != nil {
				return engine.Undefined(), err
			}
			return engine.ObjectValue(o), nil
		}), nil
	}

	b, err := s.bufferSource(moejs.Arg(args, 0), "WebAssembly.instantiate()")
	if err != nil {
		return engine.Undefined(), err
	}
	m, err := s.compile(ctx, b)
	var inst *wasmInstance
	if err == nil {
		inst, err = s.instantiate(ctx, m, moejs.Arg(args, 1))
	}
	return s.promise(func() (engine.Value, error) {
		if err != nil {
			return engine.Undefined(), err
		}
		modObj, err := s.newObject(s.moduleClass, nil, m)
		if err != nil {
			return engine.Undefined(), err
		}
		instObj, err := s.newObject(s.instanceClass, nil, inst)
		if err != nil {
			return engine.Undefined(), err
		}
		result := s.realm.NewObject()
		result.SetProp(s.realm, s.realm.KeyFromGoString("module"), engine.ObjectValue(modObj))
		result.SetProp(s.realm, s.realm.KeyFromGoString("instance"), engine.ObjectValue(instObj))
		return engine.ObjectValue(result), nil
	}), nil
}

func (s *wasmState) validateFunc(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	b, err := s.bufferSource(moejs.Arg(args, 0), "WebAssembly.validate()")
	if err != nil {
		return engine.Undefined(), err
	}
	cm, err := s.wr.CompileModule(s.rt.currentContext(), b)
	if err != nil {
		return engine.Bool(false), nil
	}
	_ = cm.Close(s.rt.currentContext())
	return engine.Bool(true), nil
}

func (s *wasmState) instantiate(ctx context.Context, m *wasmModule, importObject engine.Value) (*wasmInstance, error) {
	resolved, err := s.prepareImports(ctx, m, importObject)
	if err != nil {
		return nil, err
	}
	ictx := ctx
	if len(resolved) > 0 {
		ictx = experimental.WithImportResolver(ctx, func(name string) api.Module {
			return resolved[name]
		})
	}
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
	if err != nil {
		class := s.linkErrClass
		if m.info.hasStart {
			class = s.runtimeErrClass
		}
		return nil, s.throwError(class, err.Error())
	}
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
	inst := &wasmInstance{state: s, module: m, api: mod, name: name}
	inst.exports = s.buildExports(inst)
	return inst, nil
}

func (s *wasmState) moduleExportsFunc(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	m, ok := s.moduleFromValue(moejs.Arg(args, 0))
	if !ok {
		return engine.Undefined(), r.TypeError("WebAssembly.Module.exports(): argument 0 must be a WebAssembly.Module")
	}
	out := make([]engine.Value, 0, len(m.info.exports))
	for _, e := range m.info.exports {
		o := r.NewObject()
		o.SetProp(r, r.KeyFromGoString("name"), engine.StringValue(r.InternGoString(e.name)))
		o.SetProp(r, r.KeyFromGoString("kind"), engine.StringValue(r.InternGoString(wasmKindName(e.kind))))
		out = append(out, engine.ObjectValue(o))
	}
	return engine.ObjectValue(r.NewArrayFromSlice(out)), nil
}

func (s *wasmState) moduleImportsFunc(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	m, ok := s.moduleFromValue(moejs.Arg(args, 0))
	if !ok {
		return engine.Undefined(), r.TypeError("WebAssembly.Module.imports(): argument 0 must be a WebAssembly.Module")
	}
	out := make([]engine.Value, 0, len(m.info.imports))
	for _, imp := range m.info.imports {
		o := r.NewObject()
		o.SetProp(r, r.KeyFromGoString("module"), engine.StringValue(r.InternGoString(imp.module)))
		o.SetProp(r, r.KeyFromGoString("name"), engine.StringValue(r.InternGoString(imp.name)))
		o.SetProp(r, r.KeyFromGoString("kind"), engine.StringValue(r.InternGoString(wasmKindName(imp.kind))))
		out = append(out, engine.ObjectValue(o))
	}
	return engine.ObjectValue(r.NewArrayFromSlice(out)), nil
}

func (s *wasmState) moduleCustomSectionsFunc(r *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
	m, ok := s.moduleFromValue(moejs.Arg(args, 0))
	if !ok {
		return engine.Undefined(), r.TypeError("WebAssembly.Module.customSections(): argument 0 must be a WebAssembly.Module")
	}
	name, err := r.ToString(moejs.Arg(args, 1))
	if err != nil {
		return engine.Undefined(), err
	}
	want := name.GoString()
	var out []engine.Value
	for _, sec := range m.compiled.CustomSections() {
		if sec.Name() != want {
			continue
		}
		buf, err := r.NewArrayBuffer(sec.Data())
		if err != nil {
			return engine.Undefined(), err
		}
		out = append(out, engine.ObjectValue(buf))
	}
	return engine.ObjectValue(r.NewArrayFromSlice(out)), nil
}

// namespacePlan is one import namespace (the object at importObject[module])
// prepared for instantiation.
type namespacePlan struct {
	name       string
	funcs      []nsFunc
	memory     *wasmMemory
	memoryName string
	globals    []nsGlobal
}

type nsFunc struct {
	fd      api.FunctionDefinition
	js      engine.Value
	field   string
	impName string
	params  []api.ValueType
	results []api.ValueType
}

type nsGlobal struct {
	name string
	g    *wasmGlobal
}

// prepareImports checks every import, builds host modules for JS functions,
// and builds synthetic namespace modules that re-export the host functions,
// memories and globals under the names the target imports.
func (s *wasmState) prepareImports(ctx context.Context, m *wasmModule, importObject engine.Value) (map[string]api.Module, error) {
	if len(m.info.imports) == 0 {
		return nil, nil
	}
	plans := make(map[string]*namespacePlan)
	var order []string
	getPlan := func(name string) *namespacePlan {
		if p := plans[name]; p != nil {
			return p
		}
		p := &namespacePlan{name: name}
		plans[name] = p
		order = append(order, name)
		return p
	}

	fds := m.compiled.ImportedFunctions()
	fi := 0
	for i, imp := range m.info.imports {
		if s.isHostNamespace(imp.module) {
			// Host-managed imports resolve through wazero's store. Keep the
			// function index aligned with ImportedFunctions(), which follows
			// the same binary import order.
			if imp.kind == wasmKindFunc {
				fi++
			}
			continue
		}
		if importObject.IsUndefined() || importObject.IsNull() {
			return nil, s.throwError(s.linkErrClass, fmt.Sprintf(
				"WebAssembly.instantiate(): import %d (module=%q name=%q) is missing", i, imp.module, imp.name))
		}
		nsVal, err := s.rt.raw.Get(importObject, imp.module)
		if err != nil {
			return nil, err
		}
		if !nsVal.IsObject() {
			return nil, s.throwError(s.linkErrClass, fmt.Sprintf(
				"WebAssembly.instantiate(): import %d (module=%q name=%q): module is not an object", i, imp.module, imp.name))
		}
		val, err := s.rt.raw.Get(nsVal, imp.name)
		if err != nil {
			return nil, err
		}
		p := getPlan(imp.module)
		switch imp.kind {
		case wasmKindFunc:
			if !engine.IsCallable(val) {
				return nil, s.throwError(s.linkErrClass, fmt.Sprintf(
					"WebAssembly.instantiate(): import %d (module=%q name=%q) is not a function", i, imp.module, imp.name))
			}
			if fi >= len(fds) {
				return nil, s.throwError(s.linkErrClass, "WebAssembly.instantiate(): imported function count mismatch")
			}
			fd := fds[fi]
			fi++
			p.funcs = append(p.funcs, nsFunc{
				fd:      fd,
				js:      val,
				impName: imp.name,
				params:  fd.ParamTypes(),
				results: fd.ResultTypes(),
			})
		case wasmKindMemory:
			mem, ok := s.memoryFromValue(val)
			if !ok {
				return nil, s.throwError(s.linkErrClass, fmt.Sprintf(
					"WebAssembly.instantiate(): import %d (module=%q name=%q) is not a WebAssembly.Memory", i, imp.module, imp.name))
			}
			if p.memory != nil {
				return nil, s.throwError(s.linkErrClass, fmt.Sprintf(
					"WebAssembly.instantiate(): module %q imports more than one memory, which the wazero backend does not support", imp.module))
			}
			p.memory = mem
			p.memoryName = imp.name
		case wasmKindGlobal:
			g, ok := s.globalFromValue(val)
			if !ok {
				return nil, s.throwError(s.linkErrClass, fmt.Sprintf(
					"WebAssembly.instantiate(): import %d (module=%q name=%q) is not a WebAssembly.Global", i, imp.module, imp.name))
			}
			p.globals = append(p.globals, nsGlobal{name: imp.name, g: g})
		case wasmKindTable:
			return nil, s.throwError(s.linkErrClass, fmt.Sprintf(
				"WebAssembly.instantiate(): import %d (module=%q name=%q): imported tables are not supported by the wazero backend", i, imp.module, imp.name))
		default:
			return nil, s.throwError(s.linkErrClass, fmt.Sprintf(
				"WebAssembly.instantiate(): import %d (module=%q name=%q): %s imports are not supported", i, imp.module, imp.name, wasmKindName(imp.kind)))
		}
	}

	resolved := make(map[string]api.Module, len(plans))
	for _, name := range order {
		p := plans[name]
		ns, err := s.buildNamespace(ctx, p)
		if err != nil {
			return nil, err
		}
		resolved[name] = ns
	}
	return resolved, nil
}

func (s *wasmState) buildNamespace(ctx context.Context, p *namespacePlan) (api.Module, error) {
	var hostName string
	if len(p.funcs) > 0 {
		hostName = s.nextName("host")
		hb := s.wr.NewHostModuleBuilder(hostName)
		for i := range p.funcs {
			f := &p.funcs[i]
			f.field = "f" + strconv.Itoa(i)
			hb.NewFunctionBuilder().
				WithName(f.field).
				WithGoModuleFunction(s.importFunc(f.js, f.params, f.results), f.params, f.results).
				Export(f.field)
		}
		cm, err := hb.Compile(ctx)
		if err != nil {
			return nil, s.throwError(s.linkErrClass, "WebAssembly.instantiate(): "+err.Error())
		}
		if _, err := s.wr.InstantiateModule(ctx, cm, wazero.NewModuleConfig().WithName(hostName).WithStartFunctions()); err != nil {
			return nil, s.throwError(s.linkErrClass, "WebAssembly.instantiate(): "+err.Error())
		}
	}

	b := &wasmModuleBuilder{}
	for i := range p.funcs {
		f := &p.funcs[i]
		ti := b.addType(wasmFuncSig{params: f.params, results: f.results})
		b.imports = append(b.imports, wasmImportSpec{
			module: hostName, name: f.field, kind: wasmKindFunc, typeIndex: ti,
		})
	}
	if p.memory != nil {
		b.imports = append(b.imports, wasmImportSpec{
			module: p.memory.supportModule, name: p.memory.supportExport,
			kind: wasmKindMemory, limits: p.memory.limits,
		})
	}
	for _, g := range p.globals {
		b.imports = append(b.imports, wasmImportSpec{
			module: g.g.supportModule, name: g.g.supportExport,
			kind: wasmKindGlobal, valueType: g.g.valueType, mutable: g.g.mutable,
		})
	}
	for i := range p.funcs {
		b.exports = append(b.exports, wasmExportSpec{name: p.funcs[i].impName, kind: wasmKindFunc, index: uint32(i)})
	}
	if p.memory != nil {
		b.exports = append(b.exports, wasmExportSpec{name: p.memoryName, kind: wasmKindMemory, index: 0})
	}
	for i, g := range p.globals {
		b.exports = append(b.exports, wasmExportSpec{name: g.name, kind: wasmKindGlobal, index: uint32(i)})
	}

	name := s.nextName("namespace")
	binary := b.build()
	cm, err := s.wr.CompileModule(ctx, binary)
	if err != nil {
		return nil, s.throwError(s.linkErrClass, "WebAssembly.instantiate(): "+err.Error())
	}
	mod, err := s.wr.InstantiateModule(ctx, cm, wazero.NewModuleConfig().WithName(name).WithStartFunctions())
	if err != nil {
		return nil, s.throwError(s.linkErrClass, "WebAssembly.instantiate(): "+err.Error())
	}
	return mod, nil
}

// installStreaming adds compileStreaming and instantiateStreaming in
// JavaScript. They accept a Response or a promise of one.
//
// The script is compiled per installation rather than cached in package
// state: RunScript evaluates it in this runtime's global object, and a
// cached compile error would otherwise leak into every later runtime.
func (s *wasmState) installStreaming(context.Context) error {
	script, err := moejs.CompileScript("jolt:webassembly-streaming", wasmStreamingSource)
	if err != nil {
		return err
	}
	_, err = s.rt.raw.RunScript(script)
	return err
}

const wasmStreamingSource = `
(function () {
  function bytesOf(source) {
    return Promise.resolve(source).then(function (response) {
      if (!response || typeof response.arrayBuffer !== "function") {
        throw new TypeError("WebAssembly streaming compilation requires a Response or a promise of one");
      }
      return response.arrayBuffer();
    });
  }
  Object.defineProperty(WebAssembly, "compileStreaming", {
    configurable: true,
    writable: true,
    enumerable: false,
    value: function compileStreaming(source) {
      return bytesOf(source).then(function (bytes) {
        return WebAssembly.compile(bytes);
      });
    }
  });
  Object.defineProperty(WebAssembly, "instantiateStreaming", {
    configurable: true,
    writable: true,
    enumerable: false,
    value: function instantiateStreaming(source, importObject) {
      return bytesOf(source).then(function (bytes) {
        return WebAssembly.compile(bytes).then(function (module) {
          return WebAssembly.instantiate(module, importObject).then(function (instance) {
            return { module: module, instance: instance };
          });
        });
      });
    }
  });
})();
`

// wasmKind is the external kind of a WebAssembly import or export.
type wasmKind byte

const (
	wasmKindFunc   wasmKind = 0x00
	wasmKindTable  wasmKind = 0x01
	wasmKindMemory wasmKind = 0x02
	wasmKindGlobal wasmKind = 0x03
	wasmKindTag    wasmKind = 0x04
)

func wasmKindName(k wasmKind) string {
	switch k {
	case wasmKindFunc:
		return "function"
	case wasmKindTable:
		return "table"
	case wasmKindMemory:
		return "memory"
	case wasmKindGlobal:
		return "global"
	case wasmKindTag:
		return "tag"
	default:
		return "unknown"
	}
}

// wasmLimits is a memory or table limits descriptor.
type wasmLimits struct {
	min    uint32
	max    uint32
	hasMax bool
	shared bool
}

// wasmGlobalInfo is a global's value type and mutability.
type wasmGlobalInfo struct {
	valueType api.ValueType
	mutable   bool
}

// wasmTableInfo is a table's element type and limits.
type wasmTableInfo struct {
	elementType api.ValueType
	limits      wasmLimits
}

// wasmImportInfo is one parsed import.
type wasmImportInfo struct {
	module    string
	name      string
	kind      wasmKind
	typeIndex uint32
	valueType api.ValueType
	mutable   bool
	limits    wasmLimits
}

// wasmExportInfo is one parsed export.
type wasmExportInfo struct {
	name  string
	kind  wasmKind
	index uint32
}

// wasmBinaryInfo is the subset of a WebAssembly module the JS API exposes.
type wasmBinaryInfo struct {
	imports  []wasmImportInfo
	exports  []wasmExportInfo
	tables   []wasmTableInfo
	memories []wasmLimits
	globals  []wasmGlobalInfo
	hasStart bool
}

// parseWasmBinary reads the import, export, table, memory and global sections
// of a binary that wazero has already validated. It does not interpret code.
func parseWasmBinary(b []byte) (*wasmBinaryInfo, error) {
	if len(b) < 8 || b[0] != 0x00 || b[1] != 0x61 || b[2] != 0x73 || b[3] != 0x6d {
		return nil, errors.New("jolt: invalid WebAssembly binary: bad magic")
	}
	if b[4] != 0x01 || b[5] != 0x00 || b[6] != 0x00 || b[7] != 0x00 {
		return nil, errors.New("jolt: invalid WebAssembly binary: unsupported version")
	}
	info := &wasmBinaryInfo{}
	r := wasmReader{b: b, pos: 8}
	for r.pos < len(r.b) {
		id, err := r.u8()
		if err != nil {
			return nil, err
		}
		size, err := r.u32()
		if err != nil {
			return nil, err
		}
		start := r.pos
		end := start + int(size)
		if end < start || end > len(r.b) {
			return nil, errors.New("jolt: invalid WebAssembly binary: section overruns the binary")
		}
		sec := wasmReader{b: r.b[:end], pos: start}
		switch id {
		case 1: // type
			// Function signatures come from wazero's definitions.
		case 2:
			if err := parseWasmImports(&sec, info); err != nil {
				return nil, err
			}
		case 3: // function
			// Defined function type indices are not needed for the JS API.
		case 4:
			if err := parseWasmTables(&sec, info); err != nil {
				return nil, err
			}
		case 5:
			if err := parseWasmMemories(&sec, info); err != nil {
				return nil, err
			}
		case 6:
			if err := parseWasmGlobals(&sec, info); err != nil {
				return nil, err
			}
		case 7:
			if err := parseWasmExports(&sec, info); err != nil {
				return nil, err
			}
		case 8:
			info.hasStart = true
		}
		r.pos = end
	}
	return info, nil
}

func parseWasmImports(r *wasmReader, info *wasmBinaryInfo) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		module, err := r.str()
		if err != nil {
			return err
		}
		name, err := r.str()
		if err != nil {
			return err
		}
		kindByte, err := r.u8()
		if err != nil {
			return err
		}
		imp := wasmImportInfo{module: module, name: name, kind: wasmKind(kindByte)}
		switch imp.kind {
		case wasmKindFunc:
			idx, err := r.u32()
			if err != nil {
				return err
			}
			imp.typeIndex = idx
		case wasmKindTable:
			elem, err := r.u8()
			if err != nil {
				return err
			}
			lim, err := r.limits()
			if err != nil {
				return err
			}
			imp.valueType = api.ValueType(elem)
			imp.limits = lim
			info.tables = append(info.tables, wasmTableInfo{elementType: api.ValueType(elem), limits: lim})
		case wasmKindMemory:
			lim, err := r.limits()
			if err != nil {
				return err
			}
			imp.limits = lim
			info.memories = append(info.memories, lim)
		case wasmKindGlobal:
			vt, err := r.u8()
			if err != nil {
				return err
			}
			mut, err := r.u8()
			if err != nil {
				return err
			}
			imp.valueType = api.ValueType(vt)
			imp.mutable = mut != 0
			info.globals = append(info.globals, wasmGlobalInfo{valueType: imp.valueType, mutable: imp.mutable})
		case wasmKindTag:
			if _, err := r.u8(); err != nil {
				return err
			}
			if _, err := r.u32(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("jolt: invalid WebAssembly binary: import %d has kind %d", i, kindByte)
		}
		info.imports = append(info.imports, imp)
	}
	return nil
}

func parseWasmTables(r *wasmReader, info *wasmBinaryInfo) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		elem, err := r.u8()
		if err != nil {
			return err
		}
		lim, err := r.limits()
		if err != nil {
			return err
		}
		info.tables = append(info.tables, wasmTableInfo{elementType: api.ValueType(elem), limits: lim})
	}
	return nil
}

func parseWasmMemories(r *wasmReader, info *wasmBinaryInfo) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		lim, err := r.limits()
		if err != nil {
			return err
		}
		info.memories = append(info.memories, lim)
	}
	return nil
}

func parseWasmGlobals(r *wasmReader, info *wasmBinaryInfo) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		vt, err := r.u8()
		if err != nil {
			return err
		}
		mut, err := r.u8()
		if err != nil {
			return err
		}
		if err := r.skipInitExpr(); err != nil {
			return err
		}
		info.globals = append(info.globals, wasmGlobalInfo{valueType: api.ValueType(vt), mutable: mut != 0})
	}
	return nil
}

func parseWasmExports(r *wasmReader, info *wasmBinaryInfo) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		name, err := r.str()
		if err != nil {
			return err
		}
		kindByte, err := r.u8()
		if err != nil {
			return err
		}
		idx, err := r.u32()
		if err != nil {
			return err
		}
		info.exports = append(info.exports, wasmExportInfo{name: name, kind: wasmKind(kindByte), index: idx})
	}
	return nil
}

// wasmReader reads the little-endian/LEB128 encoding of a wasm binary.
type wasmReader struct {
	b   []byte
	pos int
}

func (r *wasmReader) u8() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, errors.New("jolt: invalid WebAssembly binary: unexpected end")
	}
	v := r.b[r.pos]
	r.pos++
	return v, nil
}

func (r *wasmReader) u32() (uint32, error) {
	var v uint32
	var shift uint
	for i := 0; i < 5; i++ {
		b, err := r.u8()
		if err != nil {
			return 0, err
		}
		v |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, nil
		}
		shift += 7
	}
	return 0, errors.New("jolt: invalid WebAssembly binary: LEB128 is too long")
}

func (r *wasmReader) str() (string, error) {
	n, err := r.u32()
	if err != nil {
		return "", err
	}
	if int(n) > len(r.b)-r.pos {
		return "", errors.New("jolt: invalid WebAssembly binary: string overruns section")
	}
	s := string(r.b[r.pos : r.pos+int(n)])
	r.pos += int(n)
	return s, nil
}

func (r *wasmReader) limits() (wasmLimits, error) {
	flags, err := r.u32()
	if err != nil {
		return wasmLimits{}, err
	}
	lim := wasmLimits{shared: flags&0x02 != 0}
	lim.min, err = r.u32()
	if err != nil {
		return wasmLimits{}, err
	}
	if flags&0x01 != 0 {
		lim.hasMax = true
		lim.max, err = r.u32()
		if err != nil {
			return wasmLimits{}, err
		}
	}
	return lim, nil
}

func (r *wasmReader) skipLEB() error {
	for {
		b, err := r.u8()
		if err != nil {
			return err
		}
		if b&0x80 == 0 {
			return nil
		}
	}
}

// skipInitExpr skips a constant expression. The operand types are the ones
// enabled by CoreFeaturesV2 and the constant-expression allowed by it.
func (r *wasmReader) skipInitExpr() error {
	for {
		op, err := r.u8()
		if err != nil {
			return err
		}
		switch op {
		case 0x0b: // end
			return nil
		case 0x41, 0x42, 0x23, 0xd2: // i32.const, i64.const, global.get, ref.func
			if err := r.skipLEB(); err != nil {
				return err
			}
		case 0x43: // f32.const
			if _, err := r.bytes(4); err != nil {
				return err
			}
		case 0x44: // f64.const
			if _, err := r.bytes(8); err != nil {
				return err
			}
		case 0xd0: // ref.null
			if _, err := r.u8(); err != nil {
				return err
			}
		case 0x6a, 0x6b, 0x6c, 0x7c, 0x7d, 0x7e: // extended-const arithmetic
			// no immediates
		default:
			return fmt.Errorf("jolt: invalid WebAssembly binary: unsupported constant expression opcode 0x%02x", op)
		}
	}
}

func (r *wasmReader) bytes(n int) ([]byte, error) {
	if n < 0 || n > len(r.b)-r.pos {
		return nil, errors.New("jolt: invalid WebAssembly binary: value overruns section")
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v, nil
}

// wasmFuncSig is a function signature used by the synthetic import modules.
type wasmFuncSig struct {
	params  []api.ValueType
	results []api.ValueType
}

func (s wasmFuncSig) key() string {
	// Lengths are LEB128-encoded: a byte cast would alias signatures with
	// more than 255 parameters or results.
	b := appendU32(nil, uint32(len(s.params)))
	for _, t := range s.params {
		b = append(b, byte(t))
	}
	b = appendU32(b, uint32(len(s.results)))
	for _, t := range s.results {
		b = append(b, byte(t))
	}
	return string(b)
}

// wasmModuleBuilder writes the small synthetic modules used to expose host
// memories/globals and to make function imports visible under their original
// module names.
type wasmModuleBuilder struct {
	types     []wasmFuncSig
	typeIndex map[string]uint32
	imports   []wasmImportSpec
	tables    []wasmTableInfo
	memories  []wasmLimits
	globals   []wasmGlobalSpec
	exports   []wasmExportSpec
}

type wasmImportSpec struct {
	module    string
	name      string
	kind      wasmKind
	typeIndex uint32
	valueType api.ValueType
	mutable   bool
	limits    wasmLimits
}

type wasmExportSpec struct {
	name  string
	kind  wasmKind
	index uint32
}

type wasmGlobalSpec struct {
	valueType api.ValueType
	mutable   bool
	init      []byte // constant expression without the trailing end opcode
}

func (b *wasmModuleBuilder) addType(sig wasmFuncSig) uint32 {
	if b.typeIndex == nil {
		b.typeIndex = make(map[string]uint32)
	}
	k := sig.key()
	if i, ok := b.typeIndex[k]; ok {
		return i
	}
	i := uint32(len(b.types))
	b.types = append(b.types, sig)
	b.typeIndex[k] = i
	return i
}

func (b *wasmModuleBuilder) build() []byte {
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	if len(b.types) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.types)))
		for _, t := range b.types {
			p = append(p, 0x60)
			p = appendU32(p, uint32(len(t.params)))
			for _, vt := range t.params {
				p = append(p, byte(vt))
			}
			p = appendU32(p, uint32(len(t.results)))
			for _, vt := range t.results {
				p = append(p, byte(vt))
			}
		}
		out = append(out, wasmSection(1, p)...)
	}
	if len(b.imports) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.imports)))
		for _, imp := range b.imports {
			p = appendString(p, imp.module)
			p = appendString(p, imp.name)
			p = append(p, byte(imp.kind))
			switch imp.kind {
			case wasmKindFunc:
				p = appendU32(p, imp.typeIndex)
			case wasmKindTable:
				p = append(p, byte(imp.valueType))
				p = appendLimits(p, imp.limits)
			case wasmKindMemory:
				p = appendLimits(p, imp.limits)
			case wasmKindGlobal:
				p = append(p, byte(imp.valueType))
				p = append(p, boolByte(imp.mutable))
			}
		}
		out = append(out, wasmSection(2, p)...)
	}
	if len(b.tables) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.tables)))
		for _, t := range b.tables {
			p = append(p, byte(t.elementType))
			p = appendLimits(p, t.limits)
		}
		out = append(out, wasmSection(4, p)...)
	}
	if len(b.memories) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.memories)))
		for _, m := range b.memories {
			p = appendLimits(p, m)
		}
		out = append(out, wasmSection(5, p)...)
	}
	if len(b.globals) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.globals)))
		for _, g := range b.globals {
			p = append(p, byte(g.valueType), boolByte(g.mutable))
			p = append(p, g.init...)
			p = append(p, 0x0b)
		}
		out = append(out, wasmSection(6, p)...)
	}
	if len(b.exports) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.exports)))
		for _, e := range b.exports {
			p = appendString(p, e.name)
			p = append(p, byte(e.kind))
			p = appendU32(p, e.index)
		}
		out = append(out, wasmSection(7, p)...)
	}
	return out
}

func wasmSection(id byte, payload []byte) []byte {
	out := []byte{id}
	out = appendU32(out, uint32(len(payload)))
	return append(out, payload...)
}

func appendU32(dst []byte, v uint32) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			dst = append(dst, b|0x80)
		} else {
			return append(dst, b)
		}
	}
}

func appendString(dst []byte, s string) []byte {
	dst = appendU32(dst, uint32(len(s)))
	return append(dst, s...)
}

func appendLimits(dst []byte, l wasmLimits) []byte {
	var flags uint32
	if l.hasMax {
		flags |= 0x01
	}
	if l.shared {
		flags |= 0x02
	}
	dst = appendU32(dst, flags)
	dst = appendU32(dst, l.min)
	if l.hasMax {
		dst = appendU32(dst, l.max)
	}
	return dst
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func appendSLEB(dst []byte, v int64) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(dst, b)
		}
		dst = append(dst, b|0x80)
	}
}

func wasmConstI32(v int32) []byte {
	return appendSLEB([]byte{0x41}, int64(v))
}

func wasmConstI64(v int64) []byte {
	return appendSLEB([]byte{0x42}, v)
}

func wasmConstF32(v float32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(api.EncodeF32(v)))
	return append([]byte{0x43}, b[:]...)
}

func wasmConstF64(v float64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], api.EncodeF64(v))
	return append([]byte{0x44}, b[:]...)
}
