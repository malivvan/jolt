package jolt

import (
	"context"
	"fmt"
	"math"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

// Value is a handle to one JavaScript value. It belongs to the Runtime that
// produced it; it stays valid until that runtime closes. A Value of a runtime
// that is pooled by an Executor is only valid inside Executor.Run.
type Value struct {
	rt *Runtime
	v  moejs.Value
}

func (r *Runtime) wrap(v moejs.Value) *Value { return &Value{rt: r, v: v} }

// Raw returns the underlying engine value. Advanced users can pass it to
// moejs APIs directly.
func (v *Value) Raw() moejs.Value {
	if v == nil {
		return moejs.Undefined()
	}
	return v.v
}

// Runtime returns the runtime the value belongs to.
func (v *Value) Runtime() *Runtime {
	if v == nil {
		return nil
	}
	return v.rt
}

func (v *Value) check() error {
	if v == nil || v.rt == nil {
		return fmt.Errorf("jolt: nil Value")
	}
	if v.rt.closed.Load() {
		return ErrClosed
	}
	return nil
}

// ToGo converts the value to a Go value as moejs's ToGo does: undefined and
// null become nil, booleans bool, strings string, integral numbers int64,
// other numbers float64, arrays []any, objects map[string]any, Date
// time.Time, and functions or symbols the engine object itself. Config.Decode
// bounds the result.
func (v *Value) ToGo() (any, error) {
	if err := v.check(); err != nil {
		return nil, err
	}
	out, err := v.rt.raw.ToGoWith(v.v, v.rt.cfg.Decode)
	return out, wrap("decode", "", err)
}

// Into decodes the value into target, a non-nil pointer, using the same
// conversion as json.Unmarshal on the value's JSON text. Struct fields,
// json.RawMessage and custom unmarshalers work as they do with
// encoding/json.
func (v *Value) Into(target any) error {
	if err := v.check(); err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("jolt: decode into nil target")
	}
	return wrap("decode", "", v.rt.raw.UnmarshalWith(v.v, target, v.rt.cfg.Decode))
}

// String returns the engine's debugging representation of the value. It runs
// no JavaScript and never throws. For a string it is the string's contents;
// for an object it is a short debugging form.
func (v *Value) String() string {
	if v == nil {
		return "undefined"
	}
	return v.v.String()
}

// Text converts the value to a JavaScript string with ToString and returns
// it. It can run user code (toString/valueOf) and can throw.
func (v *Value) Text() (string, error) {
	if err := v.check(); err != nil {
		return "", err
	}
	s, err := v.rt.raw.Realm().ToString(v.v)
	if err != nil {
		return "", wrap("string", "", err)
	}
	return s.GoString(), nil
}

// Bool returns the value's JavaScript truthiness. It runs no user code.
func (v *Value) Bool() bool {
	if v == nil {
		return false
	}
	return engine.ToBoolean(v.v)
}

// Int converts the value to an int64. It fails when the value is not an
// integral number in range.
func (v *Value) Int() (int64, error) {
	g, err := v.ToGo()
	if err != nil {
		return 0, err
	}
	switch n := g.(type) {
	case int64:
		return n, nil
	case float64:
		// float64(math.MaxInt64) rounds up to 2^63, which is already out of
		// range, so compare with >= rather than >.
		if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) ||
			n < math.MinInt64 || n >= math.MaxInt64 {
			return 0, fmt.Errorf("jolt: %v is not an integral int64", n)
		}
		return int64(n), nil
	default:
		return 0, fmt.Errorf("jolt: %s is not a number", typeName(v.v.Type()))
	}
}

// Float converts the value to a float64.
func (v *Value) Float() (float64, error) {
	g, err := v.ToGo()
	if err != nil {
		return 0, err
	}
	switch n := g.(type) {
	case float64:
		return n, nil
	case int64:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("jolt: %s is not a number", typeName(v.v.Type()))
	}
}

// Type returns the JavaScript type name: "undefined", "null", "boolean",
// "number", "string", "symbol", "bigint" or "object".
func (v *Value) Type() string {
	if v == nil {
		return "undefined"
	}
	return typeName(v.v.Type())
}

func typeName(t engine.Type) string {
	switch t {
	case engine.TypeUndefined:
		return "undefined"
	case engine.TypeNull:
		return "null"
	case engine.TypeBoolean:
		return "boolean"
	case engine.TypeNumber:
		return "number"
	case engine.TypeString:
		return "string"
	case engine.TypeSymbol:
		return "symbol"
	case engine.TypeBigInt:
		return "bigint"
	case engine.TypeObject:
		return "object"
	default:
		return "unknown"
	}
}

// IsNull reports whether the value is null.
func (v *Value) IsNull() bool { return v != nil && v.v.IsNull() }

// IsUndefined reports whether the value is undefined.
func (v *Value) IsUndefined() bool { return v == nil || v.v.IsUndefined() }

// IsNullish reports whether the value is null or undefined.
func (v *Value) IsNullish() bool { return v == nil || v.v.IsNullish() }

// IsObject reports whether the value is an object (including a function or
// array).
func (v *Value) IsObject() bool { return v != nil && v.v.IsObject() }

// IsFunction reports whether the value is callable.
func (v *Value) IsFunction() bool { return v != nil && engine.IsCallable(v.v) }

// IsArray reports whether the value is an Array.
func (v *Value) IsArray() bool { return v != nil && engine.IsArray(v.v) }

// IsPromise reports whether the value is a promise.
func (v *Value) IsPromise() bool {
	if v == nil {
		return false
	}
	_, _, ok := moejs.PromiseResult(v.v)
	return ok
}

// Get reads a property, running a getter and walking the prototype chain.
func (v *Value) Get(key string) (*Value, error) {
	if err := v.check(); err != nil {
		return nil, err
	}
	got, err := v.rt.raw.Get(v.v, key)
	return v.rt.wrap(got), wrap("get", key, err)
}

// Set writes a property. value is converted like Runtime.Set's argument.
func (v *Value) Set(key string, value any) error {
	if err := v.check(); err != nil {
		return err
	}
	return v.rt.setMember(v.v, key, value)
}

// Call calls the value as a function with this = undefined. Like Runtime.Call,
// it awaits a returned promise unless Config.DisableAwaitPromises is set; from
// a host function already running on the loop, the call is synchronous and the
// promise is returned unawaited.
func (v *Value) Call(ctx context.Context, args ...any) (*Value, error) {
	if err := v.check(); err != nil {
		return nil, err
	}
	return v.rt.callValue(ctx, "call", v, moejs.Undefined(), args, true)
}

// CallMethod reads key as a method of the value and calls it with the value
// as this.
func (v *Value) CallMethod(ctx context.Context, key string, args ...any) (*Value, error) {
	m, err := v.Get(key)
	if err != nil {
		return nil, err
	}
	if !m.IsFunction() {
		return nil, wrap("call", key, fmt.Errorf("%w: %s", ErrNotFunction, key))
	}
	return v.rt.callValue(ctx, "call", m, v.v, args, true)
}

// Await waits for the value to settle if it is a promise, and returns the
// fulfillment value. A rejected promise is returned as a *JSError. A value
// that is not a promise is returned as is.
func (v *Value) Await(ctx context.Context) (*Value, error) {
	if err := v.check(); err != nil {
		return nil, err
	}
	return v.rt.awaitValue(ctx, v)
}
