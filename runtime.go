package jolt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/eventloop"
)

// Runtime owns one moejs runtime, its event loop and its installed features.
//
// A Runtime is not safe for concurrent use: moejs requires one goroutine at a
// time, and so does jolt. Executor provides concurrency by keeping a pool of
// runtimes. Context cancellation is safe: it interrupts the call from another
// goroutine.
type Runtime struct {
	prog *program
	cfg  Config
	raw  *moejs.Runtime
	loop *eventloop.Loop

	cleanups []func() error

	closed  atomic.Bool
	closing atomic.Bool
	running atomic.Bool
	broken  atomic.Bool

	// ctx is the context of the call currently running on this runtime. Only
	// the runtime's goroutine reads it; host functions use it to receive a
	// context.
	ctx context.Context
}

// Raw returns the underlying moejs runtime for advanced use. The caller must
// respect the same single-goroutine rule.
func (r *Runtime) Raw() *moejs.Runtime { return r.raw }

// Loop returns the runtime's event loop.
func (r *Runtime) Loop() *eventloop.Loop { return r.loop }

// Config returns the configuration that created the runtime.
func (r *Runtime) Config() Config { return r.cfg }

// Stats returns the engine's counters.
func (r *Runtime) Stats() moejs.Stats { return r.raw.Stats() }

// ModuleName returns the name of the compiled program.
func (r *Runtime) ModuleName() string { return r.cfg.Name }

// Exports returns the export names of the loaded module, sorted. A classic
// script has no exports.
func (r *Runtime) Exports() []string {
	if r.prog.entryModule == nil {
		return nil
	}
	return r.prog.entryModule.Exports()
}

// Close releases the runtime: it runs OnRuntimeClose, closes the installed
// features and terminates the event loop. It returns the first errors joined.
// Calling Close more than once is safe. Close must not be called from
// JavaScript running on the runtime.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	if r.running.Load() {
		return fmt.Errorf("jolt: Close called while the runtime is running")
	}
	if !r.closing.CompareAndSwap(false, true) {
		return nil
	}
	var errs []error
	if r.cfg.OnRuntimeClose != nil {
		if err := r.cfg.OnRuntimeClose(r); err != nil {
			errs = append(errs, err)
		}
	}
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		if err := r.cleanups[i](); err != nil {
			errs = append(errs, err)
		}
	}
	r.cleanups = nil
	r.loop.Terminate()
	r.raw.ClearInterrupt()
	r.closed.Store(true)
	return errors.Join(errs...)
}

// Set installs a global variable. A dotted name walks or creates nested
// objects, as in Set("utils.env", value). Values may be funcs, maps, slices,
// structs or *Value handles. Set writes globals; module exports are read-only
// bindings.
func (r *Runtime) Set(name string, value any) error {
	return r.SetPath(name, value)
}

// SetPath is Set with an explicit dotted path.
func (r *Runtime) SetPath(path string, value any) error {
	if err := r.ready("set", path); err != nil {
		return err
	}
	if path == "" {
		return wrap("set", path, errors.New("empty path"))
	}
	raw, err := r.fromGo(value)
	if err != nil {
		return wrap("set", path, err)
	}
	parts := strings.Split(path, ".")
	for i, part := range parts {
		if part == "" {
			return wrap("set", path, fmt.Errorf("empty path element at %d", i+1))
		}
	}
	if len(parts) == 1 {
		if err := r.raw.SetGlobal(path, raw); err != nil {
			return wrap("set", path, err)
		}
		return nil
	}

	realm := r.raw.Realm()
	cur := realm.Global
	for i, part := range parts {
		key := realm.KeyFromGoString(part)
		if i == len(parts)-1 {
			if err := realm.SetV(engine.ObjectValue(cur), key, raw); err != nil {
				return wrap("set", path, err)
			}
			break
		}
		next, err := cur.GetProp(realm, key)
		if err != nil {
			return wrap("set", path, err)
		}
		if next.IsUndefined() || next.IsNull() {
			o := realm.NewObject()
			if err := cur.SetProp(realm, key, engine.ObjectValue(o)); err != nil {
				return wrap("set", path, err)
			}
			cur = o
			continue
		}
		if !next.IsObject() {
			return wrap("set", path, fmt.Errorf("%q is %s, not an object", part, typeName(next.Type())))
		}
		cur = next.AsObject()
	}
	if !r.running.Load() {
		r.raw.ResetAllocation()
	}
	return nil
}

// SetFunc installs a Go function as a global host function. Any function
// signature supported by SetFunc's reflection rules works; see the package
// documentation.
func (r *Runtime) SetFunc(name string, fn any) error {
	if err := r.ready("set", name); err != nil {
		return err
	}
	path := name
	short := name
	if i := strings.LastIndex(name, "."); i >= 0 {
		short = name[i+1:]
	}
	if path == "" || short == "" {
		return wrap("set", name, errors.New("empty function name"))
	}
	if raw, ok := fn.(moejs.Value); ok {
		if !engine.IsCallable(raw) {
			return wrap("set", name, ErrNotFunction)
		}
		return r.SetPath(path, raw)
	}
	if v, ok := fn.(*Value); ok {
		if v == nil || v.rt != r || !v.IsFunction() {
			return wrap("set", name, ErrNotFunction)
		}
		return r.SetPath(path, v.v)
	}
	nf, length, err := r.adaptFunc(short, fn)
	if err != nil {
		return wrap("set", name, err)
	}
	return r.SetPath(path, r.raw.Function(short, length, nf))
}

// SetNamespace installs a map as one global object. Nested maps, slices,
// funcs and *Value handles are converted recursively.
func (r *Runtime) SetNamespace(name string, ns map[string]any) error {
	return r.Set(name, ns)
}

// SetValue installs a *Value of this runtime as a global.
func (r *Runtime) SetValue(name string, v *Value) error {
	if v == nil || v.rt != r {
		return wrap("set", name, errors.New("value belongs to another runtime"))
	}
	return r.SetPath(name, v)
}

// Get resolves a dotted path. The first element is looked up in the loaded
// module's exports first and then in the global object; later elements are
// ordinary property reads. A missing path yields an undefined Value, not an
// error.
func (r *Runtime) Get(path string) (*Value, error) {
	if err := r.ready("get", path); err != nil {
		return nil, err
	}
	v, err := r.lookup(path)
	return r.wrap(v), wrap("get", path, err)
}

// Global reads a property of the global object.
func (r *Runtime) Global(name string) (*Value, error) {
	if err := r.ready("get", name); err != nil {
		return nil, err
	}
	v, err := r.raw.Get(engine.ObjectValue(r.raw.Realm().Global), name)
	return r.wrap(v), wrap("get", name, err)
}

// Export reads a module export by name.
func (r *Runtime) Export(name string) (*Value, error) {
	if err := r.ready("export", name); err != nil {
		return nil, err
	}
	v, ok := r.raw.Export(name)
	if !ok {
		return r.wrap(moejs.Undefined()), wrap("export", name, ErrNotFound)
	}
	return r.wrap(v), nil
}

// Func resolves a path and returns a callable handle. It fails with
// ErrNotFunction when the path names something that is not callable.
func (r *Runtime) Func(path string) (*Function, error) {
	v, err := r.Get(path)
	if err != nil {
		return nil, err
	}
	if !v.IsFunction() {
		return nil, wrap("func", path, ErrNotFunction)
	}
	return &Function{rt: r, name: path, value: v}, nil
}

// Call invokes the function at path with the given Go arguments. The first
// path element is resolved against module exports, then globals.
//
// Unless Config.DisableAwaitPromises is set, a returned promise is awaited on
// the event loop and a rejection is returned as an error. A Call made from a
// host function while the runtime is already running cannot start a nested
// event loop: it runs synchronously and returns a promise unawaited.
func (r *Runtime) Call(ctx context.Context, path string, args ...any) (*Value, error) {
	return r.invoke(ctx, "call", path, moejs.Undefined(), false, moejs.Undefined(), args, true)
}

// CallInto calls path and decodes the result into out, a non-nil pointer.
func (r *Runtime) CallInto(ctx context.Context, path string, out any, args ...any) error {
	res, err := r.Call(ctx, path, args...)
	if err != nil {
		return err
	}
	return res.Into(out)
}

// CallValue calls a JavaScript function value.
func (r *Runtime) CallValue(ctx context.Context, fn *Value, args ...any) (*Value, error) {
	if fn == nil || fn.rt != r {
		return nil, wrap("call", "", errors.New("value belongs to another runtime"))
	}
	return r.callValue(ctx, "call", fn, moejs.Undefined(), args, true)
}

// CallValueInto calls a JavaScript function value and decodes the result into
// out.
func (r *Runtime) CallValueInto(ctx context.Context, fn *Value, out any, args ...any) error {
	res, err := r.CallValue(ctx, fn, args...)
	if err != nil {
		return err
	}
	return res.Into(out)
}

// Await waits for v to settle if it is a promise. It cannot be called from a
// host function already running on the event loop, where a nested loop cannot
// run; Value.Await has the same restriction.
func (r *Runtime) Await(ctx context.Context, v *Value) (*Value, error) {
	if v == nil || v.rt != r {
		return nil, wrap("await", "", errors.New("value belongs to another runtime"))
	}
	return r.awaitValue(ctx, v)
}

// Eval compiles and runs source as a classic script in the runtime's global
// environment and returns its completion value.
func (r *Runtime) Eval(ctx context.Context, source string) (*Value, error) {
	return r.eval(ctx, source, r.cfg.TypeScript)
}

// EvalTS is Eval for TypeScript source.
func (r *Runtime) EvalTS(ctx context.Context, source string) (*Value, error) {
	return r.eval(ctx, source, true)
}

// EvalInto evaluates source and decodes its completion value into out.
func (r *Runtime) EvalInto(ctx context.Context, source string, out any) error {
	v, err := r.Eval(ctx, source)
	if err != nil {
		return err
	}
	return v.Into(out)
}

func (r *Runtime) eval(ctx context.Context, source string, ts bool) (*Value, error) {
	if err := r.ready("eval", ""); err != nil {
		return nil, err
	}
	name := r.cfg.Name + ":eval"
	script, err := compileScript(name, source, ts, r.cfg.CompileTimeout)
	if err != nil {
		return nil, err
	}
	if r.running.Load() {
		res, err := r.raw.RunScript(script)
		return r.wrap(res), wrap("eval", "", err)
	}

	var res moejs.Value
	var callErr error
	var cleanup func()
	runErr := r.onLoop(ctx, "eval", func(ctx context.Context) error {
		res, callErr = r.raw.RunScript(script)
		if callErr != nil {
			return callErr
		}
		if !r.cfg.DisableAwaitPromises {
			cleanup = r.awaitInto(res, &res, &callErr)
			return callErr
		}
		return nil
	})
	if cleanup != nil {
		cleanup()
	}
	err = callErr
	if runErr != nil {
		err = runErr
	}
	return r.wrap(res), wrap("eval", "", err)
}

// currentContext returns the context of the running call, for a host function
// that takes a context.Context parameter.
func (r *Runtime) currentContext() context.Context {
	if r.ctx != nil {
		return r.ctx
	}
	return context.Background()
}

// ready checks that the runtime can be used for op.
func (r *Runtime) ready(op, path string) error {
	if r.closed.Load() {
		return wrap(op, path, ErrClosed)
	}
	return nil
}

// lookup resolves a dotted path as Get does.
func (r *Runtime) lookup(path string) (moejs.Value, error) {
	if path == "" {
		return moejs.Undefined(), errors.New("empty path")
	}
	parts := strings.Split(path, ".")
	for i, part := range parts {
		if part == "" {
			return moejs.Undefined(), fmt.Errorf("empty path element at %d", i+1)
		}
	}
	var cur moejs.Value
	if v, ok := r.raw.Export(parts[0]); ok {
		cur = v
	} else {
		v, err := r.raw.Get(engine.ObjectValue(r.raw.Realm().Global), parts[0])
		if err != nil {
			return moejs.Undefined(), err
		}
		cur = v
	}
	for _, key := range parts[1:] {
		if !cur.IsObject() {
			return moejs.Undefined(), nil
		}
		v, err := r.raw.Get(cur, key)
		if err != nil {
			return moejs.Undefined(), err
		}
		cur = v
	}
	return cur, nil
}

// convertArgs converts Go arguments to engine values.
func (r *Runtime) convertArgs(args []any) ([]moejs.Value, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make([]moejs.Value, len(args))
	for i, arg := range args {
		v, err := r.fromGo(arg)
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", i+1, err)
		}
		out[i] = v
	}
	return out, nil
}

func (r *Runtime) callValue(ctx context.Context, op string, fn *Value, this moejs.Value, args []any, await bool) (*Value, error) {
	if fn == nil || fn.rt != r {
		return nil, wrap(op, "", errors.New("value belongs to another runtime"))
	}
	return r.invoke(ctx, op, "", fn.v, true, this, args, await)
}

// invoke is the one call path: resolve the function, convert arguments, call,
// and optionally await a promise. It runs inside the event loop, except when a
// host function calls back into JavaScript, where the loop is already running.
func (r *Runtime) invoke(ctx context.Context, op, label string, fn moejs.Value, haveFn bool, this moejs.Value, args []any, await bool) (*Value, error) {
	if err := r.ready(op, label); err != nil {
		return nil, err
	}
	if r.running.Load() {
		if !haveFn {
			v, err := r.lookup(label)
			if err != nil {
				return nil, wrap(op, label, err)
			}
			fn = v
		}
		if !engine.IsCallable(fn) {
			return nil, wrap(op, label, ErrNotFunction)
		}
		vals, err := r.convertArgs(args)
		if err != nil {
			return nil, wrap(op, label, err)
		}
		res, err := r.raw.CallFunction(fn, this, vals...)
		return r.wrap(res), wrap(op, label, r.enrich(err))
	}

	var res moejs.Value
	var callErr error
	var cleanup func()
	runErr := r.onLoop(ctx, op, func(ctx context.Context) error {
		f := fn
		if !haveFn {
			v, err := r.lookup(label)
			if err != nil {
				callErr = err
				return err
			}
			f = v
		}
		if !engine.IsCallable(f) {
			callErr = ErrNotFunction
			return callErr
		}
		vals, err := r.convertArgs(args)
		if err != nil {
			callErr = err
			return err
		}
		res, err = r.raw.CallFunction(f, this, vals...)
		if err != nil {
			callErr = err
			return err
		}
		if await && !r.cfg.DisableAwaitPromises {
			cleanup = r.awaitInto(res, &res, &callErr)
			return callErr
		}
		return nil
	})
	if cleanup != nil {
		cleanup()
	}
	err := callErr
	if runErr != nil {
		err = runErr
	}
	return r.wrap(res), wrap(op, label, err)
}

// onLoop runs fn on the runtime's goroutine with the event loop, watching ctx
// for cancellation. It clears the interrupt and releases call data before
// returning, so the runtime can be reused.
func (r *Runtime) onLoop(ctx context.Context, op string, fn func(context.Context) error) (err error) {
	if r.closed.Load() {
		return wrap(op, "", ErrClosed)
	}
	if r.running.Load() {
		return wrap(op, "", ErrReentrant)
	}
	execCtx, cancel := r.execContext(ctx)
	defer cancel()
	if cause := context.Cause(execCtx); cause != nil {
		return wrap(op, "", cause)
	}

	interrupted := make(chan struct{})
	stop := context.AfterFunc(execCtx, func() {
		r.loop.Interrupt(context.Cause(execCtx))
		close(interrupted)
	})
	r.running.Store(true)
	r.ctx = execCtx
	defer func() {
		if p := recover(); p != nil {
			r.ctx = nil
			r.running.Store(false)
			if !stop() {
				<-interrupted
			}
			r.raw.ClearInterrupt()
			r.raw.ReleaseCallData()
			r.broken.Store(true)
			panic(p)
		}
	}()
	runErr := r.loop.Run(func(*moejs.Runtime) error { return fn(execCtx) })
	r.ctx = nil
	r.running.Store(false)
	if !stop() {
		<-interrupted
	}
	r.raw.ClearInterrupt()
	r.raw.ReleaseCallData()

	if runErr == nil {
		if cause := context.Cause(execCtx); cause != nil {
			runErr = &moejs.InterruptedError{Value: cause}
		}
	}
	runErr = r.enrich(runErr)
	if runErr != nil && (isBroken(runErr) || errors.Is(runErr, ErrClosed)) {
		r.broken.Store(true)
	}
	return runErr
}

func (r *Runtime) execContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r.cfg.DefaultTimeout > 0 {
		if _, ok := ctx.Deadline(); !ok {
			return context.WithTimeout(ctx, r.cfg.DefaultTimeout)
		}
	}
	return ctx, func() {}
}

// awaitInto awaits the promise p and writes the result to res or errOut. It
// must run on the runtime's goroutine inside the event loop. The returned
// cleanup releases the loop hold when the caller stops waiting; it is safe to
// call after the promise has settled.
func (r *Runtime) awaitInto(p moejs.Value, res *moejs.Value, errOut *error) (cleanup func()) {
	state, val, ok := moejs.PromiseResult(p)
	if !ok {
		*res = p
		return nil
	}
	switch state {
	case moejs.PromiseFulfilled:
		*res = val
		return nil
	case moejs.PromiseRejected:
		*errOut = r.rejectionError(val)
		return nil
	}

	var once sync.Once
	done := r.loop.Hold()
	release := func() { once.Do(func() { done(nil) }) }

	resolveFn := r.raw.Function("resolve", 1, func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		*res = moejs.Arg(args, 0)
		release()
		return moejs.Undefined(), nil
	})
	rejectFn := r.raw.Function("reject", 1, func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		*errOut = r.rejectionError(moejs.Arg(args, 0))
		release()
		return moejs.Undefined(), nil
	})

	then, err := r.raw.Get(p, "then")
	if err != nil {
		release()
		*errOut = err
		return nil
	}
	if !engine.IsCallable(then) {
		release()
		*errOut = ErrNotFunction
		return nil
	}
	if _, err := r.raw.CallFunction(then, p, resolveFn, rejectFn); err != nil {
		release()
		*errOut = err
		return nil
	}
	return release
}

func (r *Runtime) awaitValue(ctx context.Context, v *Value) (*Value, error) {
	if err := r.ready("await", ""); err != nil {
		return nil, err
	}
	if r.running.Load() {
		return nil, wrap("await", "", ErrReentrant)
	}
	var res moejs.Value
	var callErr error
	var cleanup func()
	runErr := r.onLoop(ctx, "await", func(ctx context.Context) error {
		cleanup = r.awaitInto(v.v, &res, &callErr)
		return callErr
	})
	if cleanup != nil {
		cleanup()
	}
	err := callErr
	if runErr != nil {
		err = runErr
	}
	return r.wrap(res), wrap("await", "", err)
}

// enrich fills in the stack of a thrown JavaScript Error. moejs carries the
// stack on the Error object; copying it to the Go exception makes it available
// after the call and through JSStack.
func (r *Runtime) enrich(err error) error {
	var exc *moejs.Exception
	if errors.As(err, &exc) && exc.Stack == "" {
		exc.Stack = r.raw.StackTrace(exc)
	}
	return err
}

// rejectionError converts a promise rejection reason to a Go error.
func (r *Runtime) rejectionError(reason moejs.Value) error {
	js := &JSError{}
	if reason.IsObject() {
		js.Name = r.stringProp(reason, "name")
		js.Message = r.stringProp(reason, "message")
		js.Stack = r.stringProp(reason, "stack")
	}
	if g, err := r.raw.ToGo(reason); err == nil {
		js.Value = g
	}
	if js.Name == "" {
		js.Name = "Error"
	}
	if js.Message == "" {
		switch {
		case reason.IsUndefined():
			js.Message = "promise rejected with undefined"
		case reason.IsNull():
			js.Message = "promise rejected with null"
		default:
			if s, err := r.raw.Realm().ToString(reason); err == nil {
				js.Message = s.GoString()
			} else {
				js.Message = reason.String()
			}
		}
	}
	return js
}

func (r *Runtime) stringProp(v moejs.Value, key string) string {
	p, err := r.raw.Get(v, key)
	if err != nil || !p.IsString() {
		return ""
	}
	return p.String()
}

// setMember sets one property of a JavaScript object value.
func (r *Runtime) setMember(target moejs.Value, key string, value any) error {
	if err := r.ready("set", key); err != nil {
		return err
	}
	if !target.IsObject() {
		return wrap("set", key, errors.New("target is not an object"))
	}
	raw, err := r.fromGo(value)
	if err != nil {
		return wrap("set", key, err)
	}
	realm := r.raw.Realm()
	return wrap("set", key, realm.SetV(target, realm.KeyFromGoString(key), raw))
}

// Function is a resolved JavaScript function handle.
type Function struct {
	rt    *Runtime
	name  string
	value *Value
}

// Name returns the path the function was resolved from.
func (f *Function) Name() string { return f.name }

// Value returns the function as a Value.
func (f *Function) Value() *Value { return f.value }

// Call invokes the function.
func (f *Function) Call(ctx context.Context, args ...any) (*Value, error) {
	return f.rt.CallValue(ctx, f.value, args...)
}

// CallInto invokes the function and decodes the result into out.
func (f *Function) CallInto(ctx context.Context, out any, args ...any) error {
	return f.rt.CallValueInto(ctx, f.value, out, args...)
}
