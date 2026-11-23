package js

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"runtime/debug"
	"sort"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

var (
	errorType      = reflect.TypeOf((*error)(nil)).Elem()
	contextType    = reflect.TypeOf((*context.Context)(nil)).Elem()
	valueType      = reflect.TypeOf(moejs.Value{})
	ptrValueType   = reflect.TypeOf((*Value)(nil))
	nativeFuncType = reflect.TypeOf(moejs.NativeFunc(nil))
	realmPtrType   = reflect.TypeOf((*moejs.Realm)(nil))
	runtimePtrType = reflect.TypeOf((*Runtime)(nil))
	objectPtrType  = reflect.TypeOf((*moejs.Object)(nil))
	byteSliceType  = reflect.TypeOf([]byte(nil))
)

// adaptFunc turns a Go function into a moejs host function. It reports the
// JavaScript `length` property to use.
//
// Parameters may be context.Context, *Runtime, *moejs.Realm, moejs.Value,
// *Value, *moejs.Object, or any type moejs can decode a JavaScript value
// into. The last parameter may be variadic. Results may be (T), (T, error)
// or error.
func (r *Runtime) adaptFunc(name string, fn any) (moejs.NativeFunc, int, error) {
	if fn == nil {
		return nil, 0, fmt.Errorf("js: %w: nil func", ErrNotFunction)
	}
	fv := reflect.ValueOf(fn)
	ft := fv.Type()
	if ft.Kind() != reflect.Func {
		return nil, 0, fmt.Errorf("js: %w: %T", ErrNotFunction, fn)
	}
	// A function with the engine's own signature needs no reflection.
	if ft.ConvertibleTo(nativeFuncType) {
		nf := fv.Convert(nativeFuncType).Interface().(moejs.NativeFunc)
		return nf, ft.NumIn(), nil
	}
	if name == "" {
		if rf := runtime.FuncForPC(fv.Pointer()); rf != nil {
			name = baseName(rf.Name())
		}
	}

	injected := 0
	if ft.NumIn() > 0 {
		switch ft.In(0) {
		case contextType, runtimePtrType, realmPtrType:
			injected = 1
		}
	}
	length := ft.NumIn() - injected
	if ft.IsVariadic() {
		length--
	}
	if length < 0 {
		length = 0
	}

	nout := ft.NumOut()
	switch {
	case nout == 0:
	case nout == 1 && ft.Out(0).Implements(errorType):
	case nout == 1:
	case nout == 2 && ft.Out(1).Implements(errorType):
	default:
		return nil, 0, fmt.Errorf("js: %w: %s returns %d values; want (T), (T, error) or error",
			ErrNotFunction, ft, nout)
	}

	nf := func(realm *moejs.Realm, _ moejs.Value, args []moejs.Value) (res moejs.Value, err error) {
		defer func() {
			if p := recover(); p != nil {
				res = moejs.Undefined()
				err = &PanicError{Func: name, Value: p, Stack: debug.Stack()}
			}
		}()
		in := make([]reflect.Value, 0, ft.NumIn())
		ai := 0
		for i := 0; i < ft.NumIn(); i++ {
			pt := ft.In(i)
			if i == 0 && injected == 1 {
				switch pt {
				case contextType:
					in = append(in, reflect.ValueOf(r.currentContext()))
				case runtimePtrType:
					in = append(in, reflect.ValueOf(r))
				case realmPtrType:
					in = append(in, reflect.ValueOf(realm))
				}
				continue
			}
			if ft.IsVariadic() && i == ft.NumIn()-1 {
				elem := pt.Elem()
				n := len(args) - ai
				if n < 0 {
					n = 0
				}
				slice := reflect.MakeSlice(pt, n, n)
				for j := 0; j < n; j++ {
					v, cerr := r.paramValue(args[ai+j], elem, i)
					if cerr != nil {
						return moejs.Undefined(), cerr
					}
					slice.Index(j).Set(v)
				}
				ai = len(args)
				in = append(in, slice)
				continue
			}
			arg := moejs.Undefined()
			if ai < len(args) {
				arg = args[ai]
			}
			ai++
			v, cerr := r.paramValue(arg, pt, i)
			if cerr != nil {
				return moejs.Undefined(), cerr
			}
			in = append(in, v)
		}

		var out []reflect.Value
		if ft.IsVariadic() {
			out = fv.CallSlice(in)
		} else {
			out = fv.Call(in)
		}
		switch nout {
		case 0:
			return moejs.Undefined(), nil
		case 1:
			if ft.Out(0).Implements(errorType) {
				if e := errorValue(out[0]); e != nil {
					return moejs.Undefined(), e
				}
				return moejs.Undefined(), nil
			}
			return r.returnValue(name, out[0], ft.Out(0))
		default:
			if e := errorValue(out[1]); e != nil {
				return moejs.Undefined(), e
			}
			return r.returnValue(name, out[0], ft.Out(0))
		}
	}
	return nf, length, nil
}

// paramValue converts one JavaScript argument to a Go parameter of type t.
func (r *Runtime) paramValue(arg moejs.Value, t reflect.Type, index int) (reflect.Value, error) {
	switch t {
	case valueType:
		return reflect.ValueOf(arg), nil
	case ptrValueType:
		return reflect.ValueOf(&Value{rt: r, v: arg}), nil
	case objectPtrType:
		if arg.IsObject() {
			return reflect.ValueOf(arg.AsObject()), nil
		}
		return reflect.Zero(t), fmt.Errorf("js: argument %d is not an object", index+1)
	case realmPtrType:
		return reflect.ValueOf(r.raw.Realm()), nil
	case runtimePtrType:
		return reflect.ValueOf(r), nil
	case contextType:
		return reflect.ValueOf(r.currentContext()), nil
	}

	// A JavaScript function becomes a Go func the host can call back, so a
	// host function can take a callback. The engine's decoder does not do
	// this, and a JSON round trip would lose the function entirely.
	if t.Kind() == reflect.Func {
		if !engine.IsCallable(arg) {
			return reflect.Zero(t), fmt.Errorf("js: argument %d is not a function", index+1)
		}
		return r.funcValue(arg, t, index)
	}

	// The engine's decoder understands JS values, JSON-shaped Go types and
	// structs. It is also the fastest path for the common cases.
	p := reflect.New(t)
	if err := r.raw.ToGoInto(arg, p.Interface()); err == nil {
		return p.Elem(), nil
	}

	// Fall back to ToGo and Go-side conversion, then to a JSON round trip for
	// structs and named containers.
	g, gerr := r.raw.ToGo(arg)
	if gerr == nil {
		if cv, ok := convertGoValue(g, t); ok {
			return cv, nil
		}
		data, jerr := json.Marshal(g)
		if jerr == nil {
			q := reflect.New(t)
			if json.Unmarshal(data, q.Interface()) == nil {
				return q.Elem(), nil
			}
		}
	}
	if gerr != nil {
		return reflect.Zero(t), fmt.Errorf("js: argument %d: cannot convert JavaScript %s to %s: %w",
			index+1, typeName(arg.Type()), t, gerr)
	}
	return reflect.Zero(t), fmt.Errorf("js: argument %d: cannot convert JavaScript %s to %s",
		index+1, typeName(arg.Type()), t)
}

func convertGoValue(g any, t reflect.Type) (reflect.Value, bool) {
	if g == nil {
		switch t.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
			return reflect.Zero(t), true
		}
		return reflect.Zero(t), false
	}
	rv := reflect.ValueOf(g)
	if rv.Type().AssignableTo(t) {
		return rv, true
	}
	if t.Kind() == reflect.Interface && rv.Type().Implements(t) {
		return rv, true
	}
	if isNumericKind(rv.Kind()) && isNumericKind(t.Kind()) {
		return rv.Convert(t), true
	}
	if t == byteSliceType && rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
		return rv.Convert(t), true
	}
	return reflect.Value{}, false
}

func isNumericKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// funcValue wraps the JavaScript function arg as a Go func of type t, so a
// host function can call a callback JavaScript passed it. The returned func
// calls arg with this = undefined and converts the result back to t's result
// type. It must be called on the runtime's goroutine, like any other call.
func (r *Runtime) funcValue(arg moejs.Value, t reflect.Type, index int) (reflect.Value, error) {
	if t.IsVariadic() {
		return reflect.Zero(t), fmt.Errorf("js: argument %d: cannot convert a JavaScript function to variadic %s", index+1, t)
	}
	nout := t.NumOut()
	switch {
	case nout == 0:
	case nout == 1 && t.Out(0).Implements(errorType):
	case nout == 1:
	case nout == 2 && t.Out(1).Implements(errorType):
	default:
		return reflect.Zero(t), fmt.Errorf("js: argument %d: cannot convert a JavaScript function to %s; want (T), (T, error) or error results", index+1, t)
	}
	fn := reflect.MakeFunc(t, func(args []reflect.Value) []reflect.Value {
		vals := make([]moejs.Value, len(args))
		for i, a := range args {
			v, err := r.fromGo(a.Interface())
			if err != nil {
				return funcResults(t, nout, reflect.Zero(t.Out(0)), err)
			}
			vals[i] = v
		}
		res, err := r.raw.CallFunction(arg, moejs.Undefined(), vals...)
		if err != nil {
			return funcResults(t, nout, reflect.Zero(t.Out(0)), r.enrich(err))
		}
		if nout == 0 || (nout == 1 && t.Out(0).Implements(errorType)) {
			return funcResults(t, nout, reflect.Value{}, nil)
		}
		out, cerr := r.paramValue(res, t.Out(0), 0)
		if cerr != nil {
			return funcResults(t, nout, reflect.Zero(t.Out(0)), cerr)
		}
		return funcResults(t, nout, out, nil)
	})
	return fn, nil
}

// funcResults builds the result slice a reflected func returns, placing err in
// the trailing error result when the signature has one.
func funcResults(t reflect.Type, nout int, v reflect.Value, err error) []reflect.Value {
	switch {
	case nout == 0:
		return nil
	case nout == 1 && t.Out(0).Implements(errorType):
		return []reflect.Value{errorReflect(err)}
	case nout == 1:
		return []reflect.Value{v}
	default:
		return []reflect.Value{v, errorReflect(err)}
	}
}

func errorReflect(err error) reflect.Value {
	if err == nil {
		return reflect.Zero(errorType)
	}
	return reflect.ValueOf(&err).Elem()
}

// returnValue converts a Go result to a JavaScript value.
func (r *Runtime) returnValue(name string, v reflect.Value, t reflect.Type) (moejs.Value, error) {
	if t == valueType {
		return v.Interface().(moejs.Value), nil
	}
	if t == ptrValueType {
		if v.IsNil() {
			return moejs.Undefined(), nil
		}
		p := v.Interface().(*Value)
		if p == nil || p.rt != r {
			return moejs.Undefined(), fmt.Errorf("js: %s returned a *Value of another runtime", name)
		}
		return p.v, nil
	}
	if t == objectPtrType {
		if v.IsNil() {
			return moejs.Undefined(), nil
		}
		return engine.ObjectValue(v.Interface().(*moejs.Object)), nil
	}
	if !v.IsValid() {
		return moejs.Undefined(), nil
	}
	return r.fromGo(v.Interface())
}

func errorValue(v reflect.Value) error {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		if v.IsNil() {
			return nil
		}
	}
	if e, ok := v.Interface().(error); ok {
		return e
	}
	return nil
}

// fromGo converts a Go value for JavaScript, descending into maps, slices and
// funcs so that host functions can be exposed without being wrapped by hand.
// Values without funcs or *Value handles fall back to moejs's FromGo
// conversion, including its lazy map conversion and JSON encoding of structs.
func (r *Runtime) fromGo(v any) (moejs.Value, error) {
	switch x := v.(type) {
	case nil:
		return r.raw.FromGo(nil)
	case moejs.Value:
		if x.IsObject() && x.AsObject().HoldsCode() && r.raw.Realm().IsForeign(x) {
			return moejs.Undefined(), moejs.ErrForeign
		}
		return x, nil
	case *Value:
		if x == nil {
			return moejs.Null(), nil
		}
		if x.rt != r {
			return moejs.Undefined(), fmt.Errorf("js: *Value belongs to another runtime")
		}
		return x.v, nil
	case moejs.NativeFunc:
		return r.raw.FromGo(x)
	case *moejs.Object:
		if x == nil {
			return moejs.Null(), nil
		}
		if x.HoldsCode() && r.raw.Realm().IsForeign(engine.ObjectValue(x)) {
			return moejs.Undefined(), moejs.ErrForeign
		}
		return engine.ObjectValue(x), nil
	case map[string]any:
		return r.fromGoMap(x)
	case []any:
		return r.fromGoSlice(x)
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Func:
		nf, _, err := r.adaptFunc("", v)
		if err != nil {
			return moejs.Undefined(), err
		}
		return r.raw.FromGo(nf)
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			if rv.IsNil() {
				return r.raw.FromGo(nil)
			}
			realm := r.raw.Realm()
			o := realm.NewObject()
			keys := rv.MapKeys()
			names := make([]string, 0, len(keys))
			for _, k := range keys {
				names = append(names, k.String())
			}
			sort.Strings(names)
			for _, k := range names {
				jv, err := r.fromGo(rv.MapIndex(reflect.ValueOf(k).Convert(rv.Type().Key())).Interface())
				if err != nil {
					return moejs.Undefined(), err
				}
				if err := o.SetProp(realm, realm.KeyFromGoString(k), jv); err != nil {
					return moejs.Undefined(), err
				}
			}
			return engine.ObjectValue(o), nil
		}
	case reflect.Slice:
		if rv.Type() != byteSliceType {
			if rv.IsNil() {
				return r.raw.FromGo(nil)
			}
			vals := make([]moejs.Value, rv.Len())
			for i := range vals {
				jv, err := r.fromGo(rv.Index(i).Interface())
				if err != nil {
					return moejs.Undefined(), err
				}
				vals[i] = jv
			}
			return engine.ObjectValue(r.raw.Realm().NewArrayFromSlice(vals)), nil
		}
	}
	return r.raw.FromGo(v)
}

func (r *Runtime) fromGoMap(m map[string]any) (moejs.Value, error) {
	if m == nil {
		return r.raw.FromGo(nil)
	}
	realm := r.raw.Realm()
	o := realm.NewObject()
	for _, k := range sortedKeys(m) {
		jv, err := r.fromGo(m[k])
		if err != nil {
			return moejs.Undefined(), err
		}
		if err := o.SetProp(realm, realm.KeyFromGoString(k), jv); err != nil {
			return moejs.Undefined(), err
		}
	}
	return engine.ObjectValue(o), nil
}

func (r *Runtime) fromGoSlice(s []any) (moejs.Value, error) {
	if s == nil {
		return r.raw.FromGo(nil)
	}
	vals := make([]moejs.Value, len(s))
	for i, v := range s {
		jv, err := r.fromGo(v)
		if err != nil {
			return moejs.Undefined(), err
		}
		vals[i] = jv
	}
	return engine.ObjectValue(r.raw.Realm().NewArrayFromSlice(vals)), nil
}
