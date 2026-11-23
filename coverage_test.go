package js

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MoeForge/moejs"
)

func covExec(t *testing.T, cfg Config) *Executor {
	t.Helper()
	ex, err := New(cfg)
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

func covRuntime(t *testing.T, cfg Config) *Runtime {
	t.Helper()
	rt, err := NewRuntime(cfg)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func TestCoverageSourceModeString(t *testing.T) {
	want := map[SourceMode]string{
		ModeAuto:   "auto",
		ModeModule: "module",
		ModeScript: "script",
	}
	for mode, s := range want {
		if got := mode.String(); got != s {
			t.Fatalf("%d.String() = %q, want %q", mode, got, s)
		}
	}
	if got := SourceMode(99).String(); got != "SourceMode(99)" {
		t.Fatalf("invalid String() = %q", got)
	}
}

func TestCoverageConfigValidation(t *testing.T) {
	bad := []Config{
		{},
		{Source: "1", SourceFile: "x.js"},
		{Source: "1", Mode: 99},
		{Source: "1", MemoryLimit: -1},
		{Source: "1", PoolSize: -1},
		{Source: "1", MaxIdle: -1},
		{Source: "1", WebAssembly: WebAssemblyOptions{MemoryLimitPages: 65537}},
	}
	for i, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Fatalf("bad config %d was accepted", i)
		}
	}
}

func TestCoverageConfigDefaultsAndClones(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "program.js")
	if err := os.WriteFile(file, []byte("export const x = 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	modules := map[string]string{"m": "export const m = 1;"}
	globals := map[string]any{"g": 1}
	functions := map[string]any{"f": func() int { return 1 }}
	features := []Feature{FeatureFunc(func(*Env) (func() error, error) { return nil, nil })}
	args := []string{"a"}
	env := map[string]string{"K": "V"}
	ex := covExec(t, Config{
		SourceFile: file,
		Modules:    modules,
		Globals:    globals,
		Functions:  functions,
		Features:   features,
		WebAssembly: WebAssemblyOptions{
			WASI: WebAssemblyWASIOptions{Args: args, Env: env},
		},
	})
	cfg := ex.Config()
	if cfg.Name != "program.js" {
		t.Fatalf("Name = %q, want program.js", cfg.Name)
	}
	if cfg.PoolSize < 1 || cfg.MaxIdle != cfg.PoolSize {
		t.Fatalf("pool defaults = %d/%d", cfg.PoolSize, cfg.MaxIdle)
	}
	if len(cfg.Features) != 1 || len(cfg.Modules) != 1 {
		t.Fatalf("cloned feature/module counts = %d/%d", len(cfg.Features), len(cfg.Modules))
	}
	modules["m"] = "changed"
	globals["g"] = 2
	delete(functions, "f")
	args[0] = "changed"
	env["K"] = "changed"
	cfg = ex.Config()
	if cfg.Modules["m"] == "changed" || cfg.Globals["g"] == 2 || len(cfg.Functions) != 1 ||
		cfg.WebAssembly.WASI.Args[0] != "a" || cfg.WebAssembly.WASI.Env["K"] != "V" {
		t.Fatalf("config did not deep-copy its maps/slices: %+v", cfg)
	}

	ex2 := covExec(t, Config{Source: "1"})
	if got := ex2.Config().Name; got != "js.js" {
		t.Fatalf("default Name = %q", got)
	}
	if _, err := New(Config{SourceFile: filepath.Join(dir, "missing.js")}); err == nil {
		t.Fatalf("missing source file was accepted")
	}
	// A .ts source file is compiled as TypeScript by name.
	ts := filepath.Join(dir, "typed.ts")
	if err := os.WriteFile(ts, []byte("export const n: number = 2;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{SourceFile: ts}); err != nil {
		t.Fatalf("TypeScript source file: %v", err)
	}
}

func TestCoverageHelpers(t *testing.T) {
	if baseName("a/b/c.js") != "c.js" || baseName(`a\b\c.js`) != "c.js" || baseName("c.js") != "c.js" {
		t.Fatalf("baseName failed")
	}
	if !isTypeScriptName("A.TS") || isTypeScriptName("a.js") {
		t.Fatalf("isTypeScriptName failed")
	}
	if !hasExtension("a.ts", ".ts") || hasExtension("a", ".ts") {
		t.Fatalf("hasExtension failed")
	}
	if !equalFold("AbC", "aBc") || equalFold("a", "ab") || equalFold("a", "b") {
		t.Fatalf("equalFold failed")
	}
	if m := cloneStringMap(nil); m != nil {
		t.Fatalf("cloneStringMap(nil) = %v", m)
	}
	if m := cloneAnyMap(nil); m != nil {
		t.Fatalf("cloneAnyMap(nil) = %v", m)
	}
	if s := cloneSlice[int](nil); s != nil {
		t.Fatalf("cloneSlice(nil) = %v", s)
	}
	if s := cloneSlice([]int{1, 2}); len(s) != 2 {
		t.Fatalf("cloneSlice = %v", s)
	}

	if (WebAssemblyWASIOptions{}).configured() || (WebAssemblyWASIOptions{Disable: true}).configured() {
		t.Fatalf("empty/Disable-only WASI options reported configured")
	}
	configured := []WebAssemblyWASIOptions{
		{Args: []string{"a"}},
		{Env: map[string]string{"K": "V"}},
		{FS: fstest.MapFS{}},
		{Stdin: strings.NewReader("")},
		{Stdout: io.Discard},
		{Stderr: io.Discard},
		{Rand: strings.NewReader("")},
		{RunStart: true},
	}
	for i, o := range configured {
		if !o.configured() {
			t.Fatalf("WASI option %d not reported configured", i)
		}
	}

	cfg := Config{Source: "1", FS: NoFS{}, Network: NoNetwork{}, Features: []Feature{NoFS{}}}
	if got := cfg.allFeatures(); len(got) != 3 {
		t.Fatalf("allFeatures = %d", len(got))
	}
	if got := (Config{Source: "1"}).allFeatures(); len(got) != 0 {
		t.Fatalf("allFeatures(empty) = %d", len(got))
	}
}

func TestCoverageErrorTypes(t *testing.T) {
	if wrap("op", "p", nil) != nil {
		t.Fatalf("wrap(nil) != nil")
	}
	withPath := &Error{Op: "call", Path: "f", Err: io.EOF}
	if got := withPath.Error(); got != "js: call f: EOF" {
		t.Fatalf("Error() = %q", got)
	}
	if got := wrap("eval", "", io.EOF).Error(); got != "js: eval: EOF" {
		t.Fatalf("wrap Error() = %q", got)
	}
	if !errors.Is(wrap("call", "f", io.EOF), io.EOF) {
		t.Fatalf("wrapped error does not unwrap")
	}
	if got := (&JSError{Name: "TypeError", Message: "bad"}).Error(); got != "TypeError: bad" {
		t.Fatalf("JSError = %q", got)
	}
	if got := (&JSError{Value: 7}).Error(); got != "Error: 7" {
		t.Fatalf("JSError fallback = %q", got)
	}
	if got := (&JSError{Name: "E"}).Error(); got != "E: <nil>" {
		t.Fatalf("JSError nil value = %q", got)
	}
	if got := (&PanicError{Func: "f", Value: "boom"}).Error(); got != "js: panic in f: boom" {
		t.Fatalf("PanicError = %q", got)
	}
	if got := (&PanicError{Value: "boom"}).Error(); got != "js: panic: boom" {
		t.Fatalf("PanicError no func = %q", got)
	}

	exc := &moejs.Exception{Value: moejs.Number(1)}
	wrapped := wrap("call", "f", exc)
	if !IsException(wrapped) {
		t.Fatalf("IsException(wrapped) = false")
	}
	if got, ok := AsException(wrapped); !ok || got != exc {
		t.Fatalf("AsException = %v, %v", got, ok)
	}
	if _, ok := AsException(io.EOF); ok {
		t.Fatalf("AsException(io.EOF) = true")
	}
	deadline := &moejs.InterruptedError{Value: context.DeadlineExceeded}
	if !IsInterrupted(deadline) || !IsTimeout(deadline) || IsCanceled(deadline) {
		t.Fatalf("timeout classification failed")
	}
	if !IsTimeout(wrap("call", "f", context.DeadlineExceeded)) {
		t.Fatalf("wrapped deadline not a timeout")
	}
	canceled := &moejs.InterruptedError{Value: context.Canceled}
	if !IsCanceled(canceled) || IsTimeout(canceled) || !IsCanceled(wrap("call", "f", context.Canceled)) {
		t.Fatalf("cancel classification failed")
	}
	if !IsMemoryLimit(fmt.Errorf("outer: %w", moejs.ErrMemoryLimit)) {
		t.Fatalf("IsMemoryLimit = false")
	}
	if IsBreak := isBroken(nil); IsBreak {
		t.Fatalf("isBroken(nil) = true")
	}
	if !isBroken(deadline) || !isBroken(moejs.ErrMemoryLimit) {
		t.Fatalf("isBroken did not classify interrupt/memory errors")
	}
	if isBroken(exc) {
		t.Fatalf("isBroken(exception) = true")
	}

	if got := JSStack(&moejs.Exception{Stack: "exc"}); got != "exc" {
		t.Fatalf("JSStack(exception) = %q", got)
	}
	if got := JSStack(&JSError{Stack: "js"}); got != "js" {
		t.Fatalf("JSStack(JSError) = %q", got)
	}
	if got := JSStack(&PanicError{Stack: []byte("go")}); got != "go" {
		t.Fatalf("JSStack(PanicError) = %q", got)
	}
	if got := JSStack(io.EOF); got != "" {
		t.Fatalf("JSStack(io.EOF) = %q", got)
	}
}

func TestCoverageExecutorSurface(t *testing.T) {
	ex := covExec(t, Config{
		Name: "exec.js",
		Source: `export const answer = 42;
export const obj = { n: 3 };
export function add(a, b) { return a + b; }`,
	})
	ctx := context.Background()
	if ex.Config().Name != "exec.js" {
		t.Fatalf("Config().Name = %q", ex.Config().Name)
	}
	got, err := ex.Get(ctx, "answer")
	if err != nil || got != int64(42) {
		t.Fatalf("Get = %#v, %v", got, err)
	}
	if got, err := ex.Get(ctx, "missing"); err != nil || got != nil {
		t.Fatalf("Get missing = %#v, %v", got, err)
	}
	var obj struct {
		N int `json:"n"`
	}
	if err := ex.GetInto(ctx, "obj", &obj); err != nil || obj.N != 3 {
		t.Fatalf("GetInto = %+v, %v", obj, err)
	}
	if got, err := ex.Eval(ctx, "1 + 1"); err != nil || got != int64(2) {
		t.Fatalf("Eval = %#v, %v", got, err)
	}
	var ev struct {
		X int `json:"x"`
	}
	if err := ex.EvalInto(ctx, "({ x: 4 })", &ev); err != nil || ev.X != 4 {
		t.Fatalf("EvalInto = %+v, %v", ev, err)
	}
	if err := ex.Run(ctx, nil); err == nil {
		t.Fatalf("Run(nil) = nil")
	}
	if _, err := ex.Call(ctx, "add", 1, 2); err != nil {
		t.Fatalf("Call add: %v", err)
	}
	if err := ex.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := ex.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := ex.Get(ctx, "answer"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close = %v", err)
	}

	panicky := covExec(t, Config{Source: "export function f() { return 1; }"})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatalf("Run did not propagate a panic")
			}
		}()
		_ = panicky.Run(ctx, func(*Runtime) error { panic("boom") })
	}()
}

func TestCoverageRuntimeSurface(t *testing.T) {
	rt := covRuntime(t, Config{
		Name: "runtime.js",
		Source: `export const value = 7;
export const obj = { n: 1.5, f() { return this.n; } };
export function id(x) { return x; }
export function later() { return Promise.resolve(5); }`,
	})
	ctx := context.Background()
	if rt.Raw() == nil || rt.Loop() == nil || rt.Config().Name != "runtime.js" || rt.ModuleName() != "runtime.js" {
		t.Fatalf("runtime accessors failed")
	}
	_ = rt.Stats()
	exports := rt.Exports()
	if len(exports) == 0 {
		t.Fatalf("Exports empty")
	}
	if g, err := rt.Global("Math"); err != nil || !g.IsObject() {
		t.Fatalf("Global(Math) = %v, %v", g, err)
	}
	if g, err := rt.Global("notThere"); err != nil || !g.IsUndefined() {
		t.Fatalf("Global(missing) = %v, %v", g, err)
	}
	value, err := rt.Get("value")
	if err != nil {
		t.Fatalf("Get value: %v", err)
	}
	if err := rt.SetValue("copy", value); err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	if got, _ := rt.Get("copy"); got == nil {
		t.Fatalf("SetValue did not install")
	}
	if err := rt.SetValue("bad", nil); err == nil {
		t.Fatalf("SetValue(nil) accepted")
	}
	other := covRuntime(t, Config{Source: "export const x = 1;"})
	foreign, _ := other.Get("x")
	if err := rt.SetValue("bad", foreign); err == nil {
		t.Fatalf("SetValue(foreign) accepted")
	}

	id, _ := rt.Get("id")
	if _, err := rt.CallValue(ctx, id, 9); err != nil {
		t.Fatalf("CallValue: %v", err)
	}
	if _, err := rt.CallValue(ctx, nil); err == nil {
		t.Fatalf("CallValue(nil) accepted")
	}
	var into int
	if err := rt.CallValueInto(ctx, id, &into, 11); err != nil || into != 11 {
		t.Fatalf("CallValueInto = %d, %v", into, err)
	}
	if _, err := rt.Await(ctx, value); err != nil {
		t.Fatalf("Await(non-promise): %v", err)
	}
	if _, err := rt.Await(ctx, foreign); err == nil {
		t.Fatalf("Await(foreign) accepted")
	}
	if _, err := rt.EvalTS(ctx, "const n: number = 2; n * 2"); err != nil {
		t.Fatalf("EvalTS: %v", err)
	}
	var ev struct {
		A int `json:"a"`
	}
	if err := rt.EvalInto(ctx, "({ a: 6 })", &ev); err != nil || ev.A != 6 {
		t.Fatalf("EvalInto = %+v, %v", ev, err)
	}

	fn, err := rt.Func("id")
	if err != nil {
		t.Fatalf("Func: %v", err)
	}
	if fn.Name() != "id" || fn.Value() == nil {
		t.Fatalf("Function accessors")
	}
	if _, err := fn.Call(ctx, 3); err != nil {
		t.Fatalf("Function.Call: %v", err)
	}
	var fnOut int
	if err := fn.CallInto(ctx, &fnOut, 4); err != nil || fnOut != 4 {
		t.Fatalf("Function.CallInto = %d, %v", fnOut, err)
	}
	if _, err := rt.Func("value"); !errors.Is(err, ErrNotFunction) {
		t.Fatalf("Func(non-function) = %v", err)
	}

	for _, path := range []string{"", "a..b"} {
		if _, err := rt.Get(path); err == nil {
			t.Fatalf("Get(%q) accepted", path)
		}
		if err := rt.SetPath(path, 1); err == nil {
			t.Fatalf("SetPath(%q) accepted", path)
		}
	}
	// A rejected path must not leave a partially created object behind.
	if v, err := rt.Eval(ctx, "typeof globalThis.a"); err != nil {
		t.Fatalf("Eval: %v", err)
	} else if s, _ := v.Text(); s != "undefined" {
		t.Fatalf("SetPath(a..b) created global a: %q", s)
	}
	if err := rt.Set("num", 1); err != nil {
		t.Fatalf("Set num: %v", err)
	}
	if err := rt.SetPath("num.x", 2); err == nil {
		t.Fatalf("SetPath through a number accepted")
	}
	if err := rt.SetPath("deep.a.b", 3); err != nil {
		t.Fatalf("SetPath nested: %v", err)
	}
	if err := rt.Set("", 1); err == nil {
		t.Fatalf("Set(empty) accepted")
	}
	if _, err := rt.Export("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Export(missing) = %v", err)
	}

	// SetFunc accepts engine values, js values and bad inputs.
	rawID, _ := rt.Get("id")
	if err := rt.SetFunc("alias", rawID.Raw()); err != nil {
		t.Fatalf("SetFunc(moejs.Value): %v", err)
	}
	if err := rt.SetFunc("aliasValue", rawID); err != nil {
		t.Fatalf("SetFunc(*Value): %v", err)
	}
	if err := rt.SetFunc("badValue", value); !errors.Is(err, ErrNotFunction) {
		t.Fatalf("SetFunc(non-callable *Value) = %v", err)
	}
	if err := rt.SetFunc("badRaw", value.Raw()); !errors.Is(err, ErrNotFunction) {
		t.Fatalf("SetFunc(non-callable moejs.Value) = %v", err)
	}
	if err := rt.SetFunc("", func() {}); err == nil {
		t.Fatalf("SetFunc(empty) accepted")
	}
	if err := rt.SetFunc("a.", func() {}); err == nil {
		t.Fatalf("SetFunc(trailing dot) accepted")
	}

	// Reentrant Await and Close from a host function.
	if err := rt.SetFunc("checkReentrant", func() string {
		_, err := rt.Await(ctx, value)
		if err == nil {
			return ""
		}
		return err.Error()
	}); err != nil {
		t.Fatalf("SetFunc checkReentrant: %v", err)
	}
	got, err := rt.Eval(ctx, "checkReentrant()")
	if err != nil {
		t.Fatalf("checkReentrant: %v", err)
	}
	if !strings.Contains(got.String(), "cannot drive") {
		t.Fatalf("reentrant message = %q", got.String())
	}
	if err := rt.SetFunc("closeSelf", func() string {
		if err := rt.Close(); err != nil {
			return err.Error()
		}
		return ""
	}); err != nil {
		t.Fatalf("SetFunc closeSelf: %v", err)
	}
	got, err = rt.Eval(ctx, "closeSelf()")
	if err != nil {
		t.Fatalf("closeSelf: %v", err)
	}
	if !strings.Contains(got.String(), "while the runtime is running") {
		t.Fatalf("close message = %q", got.String())
	}

	closed := covRuntime(t, Config{Source: "export const x = 1;"})
	if err := closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := closed.Get("x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close = %v", err)
	}
}

func TestCoverageValueSurface(t *testing.T) {
	rt := covRuntime(t, Config{
		Name:                 "values.js",
		DisableAwaitPromises: true,
		Source: `export const undef = undefined;
export const nul = null;
export const b = true;
export const i = 42;
export const f = 1.5;
export const s = "text";
export const big = 9n;
export const obj = { n: 2, f() { return this.n; } };
export const arr = [1, 2];
export function fn() { return 1; }`,
	})
	ctx := context.Background()
	cases := []struct {
		name string
		want string
	}{
		{"undef", "undefined"},
		{"nul", "null"},
		{"b", "boolean"},
		{"i", "number"},
		{"s", "string"},
		{"big", "bigint"},
		{"obj", "object"},
	}
	for _, c := range cases {
		v, err := rt.Get(c.name)
		if err != nil {
			t.Fatalf("Get %s: %v", c.name, err)
		}
		if got := v.Type(); got != c.want {
			t.Fatalf("%s.Type() = %q, want %q", c.name, got, c.want)
		}
		if v.Runtime() != rt {
			t.Fatalf("%s accessors failed", c.name)
		}
	}
	undef, _ := rt.Get("undef")
	nul, _ := rt.Get("nul")
	obj, _ := rt.Get("obj")
	arr, _ := rt.Get("arr")
	fn, _ := rt.Get("fn")
	if !undef.IsUndefined() || !undef.IsNullish() || undef.IsNull() || undef.IsObject() {
		t.Fatalf("undefined predicates failed")
	}
	if !nul.IsNull() || !nul.IsNullish() || nul.IsUndefined() {
		t.Fatalf("null predicates failed")
	}
	if !obj.IsObject() || obj.IsArray() || obj.IsFunction() || !arr.IsArray() || !fn.IsFunction() {
		t.Fatalf("object predicates failed")
	}

	i, _ := rt.Get("i")
	if n, err := i.Int(); err != nil || n != 42 {
		t.Fatalf("Int = %d, %v", n, err)
	}
	if f, err := i.Float(); err != nil || f != 42 {
		t.Fatalf("Float(int) = %v, %v", f, err)
	}
	fv, _ := rt.Get("f")
	if f, err := fv.Float(); err != nil || f != 1.5 {
		t.Fatalf("Float = %v, %v", f, err)
	}
	for _, expr := range []string{"1.5", "NaN", "Infinity", "2 ** 63", "'x'", "9n"} {
		v, err := rt.Eval(ctx, expr)
		if err != nil {
			t.Fatalf("Eval %s: %v", expr, err)
		}
		if _, err := v.Int(); err == nil {
			t.Fatalf("Int(%s) accepted", expr)
		}
	}
	if err := i.Into(nil); err == nil {
		t.Fatalf("Into(nil) accepted")
	}
	if err := i.Into(&[]int{}); err == nil {
		t.Fatalf("Into(non-pointer) accepted")
	}

	s, _ := rt.Get("s")
	if text, err := s.Text(); err != nil || text != "text" {
		t.Fatalf("Text = %q, %v", text, err)
	}
	sym, err := rt.Eval(ctx, "Symbol('x')")
	if err != nil {
		t.Fatalf("Eval symbol: %v", err)
	}
	if sym.Type() != "symbol" {
		t.Fatalf("symbol Type = %q", sym.Type())
	}
	if _, err := sym.Text(); err == nil {
		t.Fatalf("Text(symbol) accepted")
	}

	if got, err := obj.Get("n"); err != nil || got.String() == "" {
		t.Fatalf("Value.Get = %v, %v", got, err)
	}
	if err := obj.Set("n", 3); err != nil {
		t.Fatalf("Value.Set: %v", err)
	}
	if got, err := obj.CallMethod(ctx, "f"); err != nil {
		t.Fatalf("CallMethod: %v", err)
	} else if n, _ := got.Int(); n != 3 {
		t.Fatalf("CallMethod result = %d", n)
	}
	if _, err := obj.CallMethod(ctx, "missing"); !errors.Is(err, ErrNotFunction) {
		t.Fatalf("CallMethod(missing) = %v", err)
	}
	if _, err := i.Call(ctx); !errors.Is(err, ErrNotFunction) {
		t.Fatalf("Call(non-function) = %v", err)
	}
	if got, err := i.Await(ctx); err != nil || got == nil {
		t.Fatalf("Await(non-promise) = %v, %v", got, err)
	}
	p, err := rt.Eval(ctx, "Promise.resolve(3)")
	if err != nil {
		t.Fatalf("Eval promise: %v", err)
	}
	if !p.IsPromise() {
		t.Fatalf("IsPromise = false")
	}
	if got, err := p.Await(ctx); err != nil {
		t.Fatalf("Await(promise): %v", err)
	} else if n, _ := got.Int(); n != 3 {
		t.Fatalf("awaited = %d", n)
	}

	var nilV *Value
	if !nilV.Raw().IsUndefined() || nilV.Runtime() != nil || nilV.String() != "undefined" ||
		nilV.Bool() || nilV.Type() != "undefined" || nilV.IsNull() || !nilV.IsUndefined() ||
		!nilV.IsNullish() || nilV.IsObject() || nilV.IsFunction() || nilV.IsArray() || nilV.IsPromise() {
		t.Fatalf("nil Value accessors failed")
	}
	if _, err := nilV.ToGo(); err == nil {
		t.Fatalf("nil Value.ToGo accepted")
	}
	if err := nilV.Into(&i); err == nil {
		t.Fatalf("nil Value.Into accepted")
	}
	if _, err := nilV.Text(); err == nil {
		t.Fatalf("nil Value.Text accepted")
	}
	if _, err := nilV.Get("x"); err == nil {
		t.Fatalf("nil Value.Get accepted")
	}
	if err := nilV.Set("x", 1); err == nil {
		t.Fatalf("nil Value.Set accepted")
	}
	if _, err := nilV.Call(ctx); err == nil {
		t.Fatalf("nil Value.Call accepted")
	}
	if _, err := nilV.CallMethod(ctx, "x"); err == nil {
		t.Fatalf("nil Value.CallMethod accepted")
	}
	if _, err := nilV.Await(ctx); err == nil {
		t.Fatalf("nil Value.Await accepted")
	}
}

func TestCoverageFuncsConversion(t *testing.T) {
	if _, ok := convertGoValue(nil, reflect.TypeOf((*int)(nil))); !ok {
		t.Fatalf("convertGoValue(nil, *int) failed")
	}
	if _, ok := convertGoValue(nil, reflect.TypeOf(0)); ok {
		t.Fatalf("convertGoValue(nil, int) succeeded")
	}
	if v, ok := convertGoValue(int64(3), reflect.TypeOf(int32(0))); !ok || v.Int() != 3 {
		t.Fatalf("numeric conversion failed")
	}
	if _, ok := convertGoValue("x", reflect.TypeOf(0)); ok {
		t.Fatalf("string->int conversion succeeded")
	}
	if v, ok := convertGoValue([]uint8{1}, reflect.TypeOf([]byte(nil))); !ok || len(v.Bytes()) != 1 {
		t.Fatalf("byte slice conversion failed")
	}
	if !isNumericKind(reflect.Int) || !isNumericKind(reflect.Float64) || isNumericKind(reflect.String) {
		t.Fatalf("isNumericKind failed")
	}

	rt := covRuntime(t, Config{
		Source: `export function run() {
  return [injectCtx(), injectRuntime(), injectRealm(), injectValues(1, 2, {}), variadic("x", 1, 2), obj({}), retValue(), retNilValue(), retObject()];
}`,
	})
	if err := rt.SetFunc("injectCtx", func(ctx context.Context) string {
		if ctx == nil {
			return "bad"
		}
		return "ctx"
	}); err != nil {
		t.Fatalf("SetFunc injectCtx: %v", err)
	}
	if err := rt.SetFunc("injectRuntime", func(r *Runtime) string {
		if r != rt {
			return "bad"
		}
		return "runtime"
	}); err != nil {
		t.Fatalf("SetFunc injectRuntime: %v", err)
	}
	if err := rt.SetFunc("injectRealm", func(realm *moejs.Realm) string {
		if realm == nil {
			return "bad"
		}
		return "realm"
	}); err != nil {
		t.Fatalf("SetFunc injectRealm: %v", err)
	}
	if err := rt.SetFunc("injectValues", func(v moejs.Value, pv *Value, obj *moejs.Object) string {
		if v.IsUndefined() || pv == nil || obj == nil {
			return "bad"
		}
		return "ok"
	}); err != nil {
		t.Fatalf("SetFunc injectValues: %v", err)
	}
	if err := rt.SetFunc("variadic", func(prefix string, nums ...int) string {
		return fmt.Sprintf("%s:%d", prefix, len(nums))
	}); err != nil {
		t.Fatalf("SetFunc variadic: %v", err)
	}
	if err := rt.SetFunc("obj", func(o *moejs.Object) string {
		if o == nil {
			return "nil"
		}
		return "obj"
	}); err != nil {
		t.Fatalf("SetFunc obj: %v", err)
	}
	// A JavaScript function passed to a Go func parameter is callable back.
	if err := rt.SetFunc("apply", func(fn func(int) int, n int) int { return fn(n) }); err != nil {
		t.Fatalf("SetFunc apply: %v", err)
	}
	if v, err := rt.Eval(context.Background(), "apply(x => x + 1, 41)"); err != nil {
		t.Fatalf("apply: %v", err)
	} else if n, _ := v.Int(); n != 42 {
		t.Fatalf("apply(x=>x+1, 41) = %v", v)
	}
	if _, err := rt.Eval(context.Background(), "apply(5, 1)"); err == nil {
		t.Fatalf("apply(5, 1) accepted a non-function")
	}
	// A callback that throws surfaces as a Go error from the callback call.
	if err := rt.SetFunc("check", func(fn func() error) error { return fn() }); err != nil {
		t.Fatalf("SetFunc check: %v", err)
	}
	if _, err := rt.Eval(context.Background(), "check(() => { throw new Error('nope') })"); err == nil {
		t.Fatalf("check accepted a throwing callback")
	}

	val := moejs.Number(5)
	if err := rt.SetFunc("retValue", func() moejs.Value { return val }); err != nil {
		t.Fatalf("SetFunc retValue: %v", err)
	}
	if err := rt.SetFunc("retNilValue", func() *Value { return nil }); err != nil {
		t.Fatalf("SetFunc retNilValue: %v", err)
	}
	if err := rt.SetFunc("retObject", func() *moejs.Object { return nil }); err != nil {
		t.Fatalf("SetFunc retObject: %v", err)
	}
	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("Call run: %v", err)
	}
	if text, err := got.ToGo(); err != nil {
		t.Fatalf("ToGo: %v", err)
	} else if fmt.Sprint(text) != fmt.Sprint([]any{"ctx", "runtime", "realm", "ok", "x:2", "obj", int64(5), nil, nil}) {
		t.Fatalf("run = %#v", text)
	}

	// Object parameter conversion failure.
	rt2 := covRuntime(t, Config{Source: `export function call() { try { obj(1); return "no"; } catch (e) { return e.message; } }`})
	if err := rt2.SetFunc("obj", func(o *moejs.Object) string { return "ok" }); err != nil {
		t.Fatalf("SetFunc obj2: %v", err)
	}
	if got, err := rt2.Call(context.Background(), "call"); err != nil {
		t.Fatalf("call: %v", err)
	} else if !strings.Contains(got.String(), "not an object") {
		t.Fatalf("object error = %q", got.String())
	}

	// Return values: nil and foreign js values.
	rt3 := covRuntime(t, Config{Source: `export function call(f) { try { return f(); } catch (e) { return e.message; } }`})
	other := covRuntime(t, Config{Source: "export const x = 1;"})
	foreign, _ := other.Get("x")
	if err := rt3.SetFunc("foreign", func() *Value { return foreign }); err != nil {
		t.Fatalf("SetFunc foreign: %v", err)
	}
	if err := rt3.SetFunc("foreignRaw", func() *Value { return &Value{rt: other, v: foreign.v} }); err != nil {
		t.Fatalf("SetFunc foreignRaw: %v", err)
	}
	if err := rt3.SetFunc("badSig", func() (int, int, int) { return 1, 2, 3 }); err == nil {
		t.Fatalf("SetFunc(bad signature) accepted")
	}
	if err := rt3.SetFunc("notFunc", 1); err == nil {
		t.Fatalf("SetFunc(non-func) accepted")
	}
	if err := rt3.SetFunc("nilFunc", nil); err == nil {
		t.Fatalf("SetFunc(nil) accepted")
	}
	fn, _ := rt3.Get("call")
	if got, err := fn.Call(context.Background(), mustJSFunc(t, rt3, "foreign")); err != nil {
		t.Fatalf("call foreign: %v", err)
	} else if !strings.Contains(got.String(), "another runtime") {
		t.Fatalf("foreign return = %q", got.String())
	}

	// fromGo containers and funcs.
	rt4 := covRuntime(t, Config{
		Name: "go.js",
		Source: `export function shape() {
  return [typeof anon, m.a, m.b, sl.length, nested.x[1], bytes instanceof ArrayBuffer];
}`,
	})
	if err := rt4.Set("m", map[string]int{"b": 2, "a": 1}); err != nil {
		t.Fatalf("Set map: %v", err)
	}
	if err := rt4.Set("sl", []int{1, 2}); err != nil {
		t.Fatalf("Set slice: %v", err)
	}
	if err := rt4.Set("nested", map[string]any{"x": []any{1, "two"}}); err != nil {
		t.Fatalf("Set nested: %v", err)
	}
	if err := rt4.Set("bytes", []byte{1, 2}); err != nil {
		t.Fatalf("Set bytes: %v", err)
	}
	if err := rt4.Set("anon", func(x int) int { return x + 1 }); err != nil {
		t.Fatalf("Set func: %v", err)
	}
	if got, err := rt4.Call(context.Background(), "shape"); err != nil {
		t.Fatalf("shape: %v", err)
	} else if text := fmt.Sprint(got.String()); text == "" {
		t.Fatalf("shape empty")
	}
	if err := rt4.Set("foreign", foreign); err == nil {
		t.Fatalf("Set(foreign *Value) accepted")
	}

	// Direct helpers.
	r := rt4
	if v, err := r.fromGoSlice([]any{1, "two"}); err != nil || v.IsUndefined() {
		t.Fatalf("fromGoSlice = %v, %v", v, err)
	}
	if v, err := r.fromGoSlice(nil); err != nil || v.IsUndefined() {
		t.Fatalf("fromGoSlice(nil) = %v, %v", v, err)
	}
	if v, err := r.fromGoMap(map[string]any{"a": 1}); err != nil || v.IsUndefined() {
		t.Fatalf("fromGoMap = %v, %v", v, err)
	}
	if _, err := r.fromGoMap(nil); err != nil {
		t.Fatalf("fromGoMap(nil) = %v", err)
	}
	if v, err := r.fromGo(nil); err != nil || v.IsUndefined() {
		t.Fatalf("fromGo(nil) = %v, %v", v, err)
	}
	if v, err := r.fromGo(&Value{rt: r, v: moejs.Null()}); err != nil || !v.IsNull() {
		t.Fatalf("fromGo(*Value) = %v, %v", v, err)
	}
	if v, err := r.fromGo((*Value)(nil)); err != nil || !v.IsNull() {
		t.Fatalf("fromGo(nil *Value) = %v, %v", v, err)
	}
	if v, err := r.fromGo((*moejs.Object)(nil)); err != nil || !v.IsNull() {
		t.Fatalf("fromGo(nil Object) = %v, %v", v, err)
	}

	// returnValue direct paths.
	rv := &Value{rt: r, v: moejs.Number(1)}
	if v, err := r.returnValue("x", reflect.ValueOf(rv), ptrValueType); err != nil || v.IsUndefined() {
		t.Fatalf("returnValue(*Value) = %v, %v", v, err)
	}
	if v, err := r.returnValue("x", reflect.Zero(ptrValueType), ptrValueType); err != nil || !v.IsUndefined() {
		t.Fatalf("returnValue(nil *Value) = %v, %v", v, err)
	}
	if _, err := r.returnValue("x", reflect.ValueOf(&Value{rt: other, v: foreign.v}), ptrValueType); err == nil {
		t.Fatalf("returnValue(foreign) accepted")
	}
	if v, err := r.returnValue("x", reflect.Zero(objectPtrType), objectPtrType); err != nil || !v.IsUndefined() {
		t.Fatalf("returnValue(nil Object) = %v, %v", v, err)
	}
	if v, err := r.returnValue("x", reflect.ValueOf(r.raw.Realm().NewObject()), objectPtrType); err != nil || !v.IsObject() {
		t.Fatalf("returnValue(Object) = %v, %v", v, err)
	}
	if v, err := r.returnValue("x", reflect.ValueOf(moejs.Number(2)), valueType); err != nil || v.IsUndefined() {
		t.Fatalf("returnValue(moejs.Value) = %v, %v", v, err)
	}
	if v, err := r.returnValue("x", reflect.Value{}, reflect.TypeOf(0)); err != nil || !v.IsUndefined() {
		t.Fatalf("returnValue(invalid) = %v, %v", v, err)
	}

	// funcValue: every result shape, plus its rejection paths.
	cbRT := covRuntime(t, Config{Source: `export const five = () => 5;
export const obj = () => ({});
export const boom = () => { throw new Error('cb'); };`})
	jsFn := mustJSFunc(t, cbRT, "five").Raw()

	// (T) result.
	ft := reflect.TypeOf(func() int { return 0 })
	if fv, err := cbRT.funcValue(jsFn, ft, 0); err != nil {
		t.Fatalf("funcValue(int): %v", err)
	} else if out := fv.Interface().(func() int)(); out != 5 {
		t.Fatalf("funcValue(int)() = %d", out)
	}
	// error result.
	ftErr := reflect.TypeOf(func() error { return nil })
	if fv, err := cbRT.funcValue(jsFn, ftErr, 0); err != nil {
		t.Fatalf("funcValue(error): %v", err)
	} else if out := fv.Interface().(func() error)(); out != nil {
		t.Fatalf("funcValue(error)() = %v", out)
	}
	// (T, error) result.
	ft2 := reflect.TypeOf(func() (int, error) { return 0, nil })
	if fv, err := cbRT.funcValue(jsFn, ft2, 0); err != nil {
		t.Fatalf("funcValue(int, error): %v", err)
	} else if n, e := fv.Interface().(func() (int, error))(); e != nil || n != 5 {
		t.Fatalf("funcValue(int, error)() = %d, %v", n, e)
	}
	// No result.
	ftVoid := reflect.TypeOf(func() {})
	if fv, err := cbRT.funcValue(jsFn, ftVoid, 0); err != nil {
		t.Fatalf("funcValue(void): %v", err)
	} else {
		fv.Interface().(func())()
	}
	// A bad result shape is rejected.
	ftBad := reflect.TypeOf(func() (int, int, int) { return 0, 0, 0 })
	if _, err := cbRT.funcValue(jsFn, ftBad, 0); err == nil {
		t.Fatalf("funcValue(bad shape) accepted")
	}
	// A variadic func type is rejected.
	ftVar := reflect.TypeOf(func(...int) int { return 0 })
	if _, err := cbRT.funcValue(jsFn, ftVar, 0); err == nil {
		t.Fatalf("funcValue(variadic) accepted")
	}
	// A callback whose result cannot convert to the Go result type errors.
	badFn := mustJSFunc(t, cbRT, "obj").Raw()
	if fv, err := cbRT.funcValue(badFn, ft, 0); err != nil {
		t.Fatalf("funcValue(int) wrap: %v", err)
	} else if out := fv.Interface().(func() int)(); out != 0 {
		t.Fatalf("funcValue(int)() with object result = %d", out)
	}
	// A callback that throws surfaces the error through the (T, error) shape.
	throwFn := mustJSFunc(t, cbRT, "boom").Raw()
	if fv, err := cbRT.funcValue(throwFn, ft2, 0); err != nil {
		t.Fatalf("funcValue(int, error) wrap: %v", err)
	} else if _, e := fv.Interface().(func() (int, error))(); e == nil {
		t.Fatalf("funcValue(int, error)() with throwing callback returned no error")
	}
	// funcResults and errorReflect directly, for the branches a call cannot
	// reach.
	if got := funcResults(ft, 1, reflect.ValueOf(7), nil); len(got) != 1 || got[0].Int() != 7 {
		t.Fatalf("funcResults(T) = %v", got)
	}
	if got := funcResults(ftVoid, 0, reflect.Value{}, nil); got != nil {
		t.Fatalf("funcResults(void) = %v", got)
	}
	if got := funcResults(ftErr, 1, reflect.Value{}, errors.New("boom")); len(got) != 1 || got[0].Interface() == nil {
		t.Fatalf("funcResults(error) = %v", got)
	}
	if got := funcResults(ft2, 2, reflect.ValueOf(1), nil); len(got) != 2 || got[1].Interface() != nil {
		t.Fatalf("funcResults(T, error) = %v", got)
	}
	if v := errorReflect(nil); v.Interface() != nil {
		t.Fatalf("errorReflect(nil) = %v", v)
	}
	if v := errorReflect(errors.New("x")); v.Interface() == nil {
		t.Fatalf("errorReflect(err) = nil")
	}
	if v, err := r.returnValue("x", reflect.ValueOf(3), reflect.TypeOf(0)); err != nil {
		t.Fatalf("returnValue(int) = %v", err)
	} else if n, _ := r.wrap(v).Int(); n != 3 {
		t.Fatalf("returnValue(int) = %v", v)
	}

	if errorValue(reflect.Value{}) != nil {
		t.Fatalf("errorValue(invalid) != nil")
	}
	if errorValue(reflect.ValueOf((*error)(nil))) != nil {
		t.Fatalf("errorValue(nil error) != nil")
	}
	if errorValue(reflect.ValueOf(io.EOF)) != io.EOF {
		t.Fatalf("errorValue(error) failed")
	}
	if errorValue(reflect.ValueOf(1)) != nil {
		t.Fatalf("errorValue(int) != nil")
	}

	// A panicking host function becomes a PanicError that JavaScript can catch.
	rt5 := covRuntime(t, Config{Source: `export function boom() { try { boom2(); return "no"; } catch (e) { return e.message; } }`})
	if err := rt5.SetFunc("boom2", func() { panic("kaboom") }); err != nil {
		t.Fatalf("SetFunc boom: %v", err)
	}
	got, err = rt5.Call(context.Background(), "boom")
	if err != nil {
		t.Fatalf("boom: %v", err)
	}
	if !strings.Contains(got.String(), "kaboom") {
		t.Fatalf("panic message = %q", got.String())
	}
}

func mustJSFunc(t *testing.T, rt *Runtime, path string) *Value {
	t.Helper()
	v, err := rt.Get(path)
	if err != nil {
		t.Fatalf("Get %s: %v", path, err)
	}
	return v
}

func TestCoverageConsole(t *testing.T) {
	var buf strings.Builder
	rt := covRuntime(t, Config{
		Console: &buf,
		Source: `export function all() {
  console.log("a", 1, { b: 2 });
  console.info("i"); console.debug("d"); console.warn("w"); console.error("e"); console.trace("t");
  console.dir({ x: 1 }); console.dir();
  console.assert(true, "no"); console.assert(false, "bad");
  const o = {}; o.self = o; console.dir(o);
}`,
	})
	if _, err := rt.Call(context.Background(), "all"); err != nil {
		t.Fatalf("all: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"a 1", "\"b\":2", "i", "d", "w", "e", "t", "{\"x\":1}", "undefined", "Assertion failed: bad"} {
		if !strings.Contains(out, want) {
			t.Fatalf("console output missing %q in %q", want, out)
		}
	}
	// A nil console discards output.
	silent := covRuntime(t, Config{Source: `console.log("quiet")`})
	if _, err := silent.Eval(context.Background(), "1"); err != nil {
		t.Fatalf("silent: %v", err)
	}
}

func TestCoverageHostFSAndNetwork(t *testing.T) {
	if f := NewHostFS("/tmp"); f.Root != "/tmp" || f.Mount != "" {
		t.Fatalf("NewHostFS = %+v", f)
	}
	cases := map[string]string{
		"example.com":       "example.com",
		"example.com:8080":  "example.com",
		"[::1]":             "::1",
		"[::1]:80":          "::1",
		"*.example.com:443": "*.example.com",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Fatalf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
	allow := AllowHosts("example.com")
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/x", nil)
	if err := allow(req); err != nil {
		t.Fatalf("allowed host rejected: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, "http://evil.test/x", nil)
	if err := allow(req); err == nil {
		t.Fatalf("evil host allowed")
	}
	if _, err := (NoFS{}).Install(nil); err != nil {
		t.Fatalf("NoFS.Install: %v", err)
	}
	if _, err := (NoNetwork{}).Install(nil); err != nil {
		t.Fatalf("NoNetwork.Install: %v", err)
	}

	rt := covRuntime(t, Config{
		Source:  `export function kind() { return typeof fetch; }`,
		Network: &HostNetwork{},
	})
	if got, err := rt.Call(context.Background(), "kind"); err != nil || got.String() != "function" {
		t.Fatalf("HostNetwork fetch = %v, %v", got, err)
	}
}

func TestCoverageModuleLoaders(t *testing.T) {
	set := SourceSet{"a.js": "A", "dir/b.js": "B", "dir/index.js": "I"}
	for _, c := range []struct {
		ref, spec, want string
	}{
		{"main.js", "a.js", "A"},
		{"main.js", "dir/b", "B"},
		{"dir/main.js", "./b", "B"},
		{"main.js", "dir", "I"},
	} {
		src, err := set.Load(c.ref, c.spec)
		if err != nil || src.Source != c.want {
			t.Fatalf("SourceSet.Load(%q,%q) = %+v, %v", c.ref, c.spec, src, err)
		}
	}
	if _, err := set.Load("main.js", "missing"); !errors.Is(err, ErrModuleNotFound) {
		t.Fatalf("SourceSet missing = %v", err)
	}
	if _, err := (SourceSet)(nil).Load("main.js", "x"); !errors.Is(err, ErrModuleNotFound) {
		t.Fatalf("nil SourceSet = %v", err)
	}

	fsys := fstest.MapFS{
		"a.js":     {Data: []byte("A")},
		"sub/b.js": {Data: []byte("B")},
	}
	l := NewFSLoader(fsys, "")
	if src, err := l.Load("main.js", "a"); err != nil || src.Source != "A" {
		t.Fatalf("FSLoader = %+v, %v", src, err)
	}
	l2 := NewFSLoader(fsys, "sub")
	if src, err := l2.Load("main.js", "b"); err != nil || src.Source != "B" {
		t.Fatalf("FSLoader(root) = %+v, %v", src, err)
	}
	// A referrer that already carries the root must not have it applied
	// twice: Config.Name is passed as the entry referrer.
	if src, err := l2.Load("sub/b.js", "b"); err != nil || src.Source != "B" {
		t.Fatalf("FSLoader(rooted referrer) = %+v, %v", src, err)
	}
	if src, err := l2.Load("sub", "b"); err != nil || src.Source != "B" {
		t.Fatalf("FSLoader(referrer == root) = %+v, %v", src, err)
	}
	if _, err := (&FSLoader{}).Load("main.js", "a"); !errors.Is(err, ErrModuleNotFound) {
		t.Fatalf("FSLoader(nil) = %v", err)
	}
	if _, err := l.Load("main.js", "missing"); !errors.Is(err, ErrModuleNotFound) {
		t.Fatalf("FSLoader missing = %v", err)
	}
	if _, err := (&FSLoader{FS: errFS{}, Root: "."}).Load("main.js", "a"); err == nil {
		t.Fatalf("FSLoader read error was swallowed")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.js"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	hl := hostLoader{dir: dir}
	if src, err := hl.Load("main.js", "./a"); err != nil || src.Source != "A" {
		t.Fatalf("hostLoader = %+v, %v", src, err)
	}
	if _, err := hl.Load("main.js", "../../etc/passwd"); !errors.Is(err, ErrModuleNotFound) {
		t.Fatalf("hostLoader traversal = %v", err)
	}
	if _, err := hl.Load("main.js", "missing"); !errors.Is(err, ErrModuleNotFound) {
		t.Fatalf("hostLoader missing = %v", err)
	}
	outside := filepath.Join(t.TempDir(), "secret.js")
	if err := os.WriteFile(outside, []byte("S"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.js")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := hl.Load("main.js", "./link"); err == nil {
			t.Fatalf("hostLoader followed a symlink outside its root")
		}
	}

	for _, c := range []struct {
		ref, spec string
		want      string
	}{
		{"", "", ""},
		{"dir/main.js", "./x", "dir/x"},
		{"main.js", "/abs/x", "abs/x"},
		{"dir/main.js", "pkg", "pkg"},
	} {
		got := importCandidates(c.ref, c.spec)
		if c.want == "" {
			continue
		}
		found := false
		for _, name := range got {
			if name == c.want {
				found = true
			}
		}
		if !found {
			t.Fatalf("importCandidates(%q,%q) = %v, missing %q", c.ref, c.spec, got, c.want)
		}
	}
	if importCandidates("", "") != nil {
		t.Fatalf("importCandidates empty = %v", importCandidates("", ""))
	}
	for _, name := range []string{"a.js", "dir/a.js"} {
		if !validModuleName(name) {
			t.Fatalf("validModuleName(%q) = false", name)
		}
	}
	for _, name := range []string{"", ".", "..", "../a", "/a"} {
		if validModuleName(name) {
			t.Fatalf("validModuleName(%q) = true", name)
		}
	}
	if got := (&program{}).fillSource(ModuleSource{}, "x.ts"); got.Name != "x.ts" || !got.TypeScript {
		t.Fatalf("fillSource = %+v", got)
	}
}

type errFS struct{}

func (errFS) Open(string) (fs.File, error) { return nil, errors.New("read boom") }

func TestCoverageModuleSources(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lib.js"), []byte("export const lib = 2;"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainFile := filepath.Join(dir, "main.js")
	if err := os.WriteFile(mainFile, []byte(`import { lib } from "./lib.js"; export function run() { return lib + 1; }`), 0o644); err != nil {
		t.Fatal(err)
	}

	ex := covExec(t, Config{
		Source:  `import { m } from "m"; export function run() { return m; }`,
		Modules: map[string]string{"m": "export const m = 5;"},
	})
	if got, err := ex.Call(context.Background(), "run"); err != nil || got != int64(5) {
		t.Fatalf("Modules = %#v, %v", got, err)
	}

	ex = covExec(t, Config{
		Source:   `import { f } from "f"; export function run() { return f; }`,
		ModuleFS: fstest.MapFS{"f.js": {Data: []byte("export const f = 6;")}},
	})
	if got, err := ex.Call(context.Background(), "run"); err != nil || got != int64(6) {
		t.Fatalf("ModuleFS = %#v, %v", got, err)
	}

	ex = covExec(t, Config{SourceFile: mainFile})
	if got, err := ex.Call(context.Background(), "run"); err != nil || got != int64(3) {
		t.Fatalf("SourceFile import = %#v, %v", got, err)
	}

	// Custom resolver: success, explicit failure, and ErrModuleNotFound fallthrough.
	resolve := func(ref moejs.Referrer, spec string) (*moejs.Module, error) {
		switch spec {
		case "virt":
			return moejs.CompileWith("virt.js", "export const v = 8;", moejs.CompileOptions{})
		case "boom":
			return nil, errors.New("resolver boom")
		default:
			return nil, ErrModuleNotFound
		}
	}
	ex = covExec(t, Config{
		Source:  `import { v } from "virt"; import { m } from "m"; export function run() { return v + m; }`,
		Modules: map[string]string{"m": "export const m = 1;"},
		Resolve: resolve,
	})
	if got, err := ex.Call(context.Background(), "run"); err != nil || got != int64(9) {
		t.Fatalf("Resolve = %#v, %v", got, err)
	}
	if _, err := New(Config{Source: `import "boom";`, Resolve: resolve}); err == nil {
		t.Fatalf("resolver error was swallowed")
	}

	// A loader error that is not ErrModuleNotFound stops resolution.
	if _, err := New(Config{
		Source: `import "x";`,
		Loader: ModuleLoaderFunc(func(string, string) (ModuleSource, error) {
			return ModuleSource{}, errors.New("loader boom")
		}),
	}); err == nil {
		t.Fatalf("loader error was swallowed")
	}

	// Configuration and feature failures abort runtime creation.
	if _, err := NewRuntime(Config{
		Source: "1",
		Features: []Feature{FeatureFunc(func(*Env) (func() error, error) {
			return nil, errors.New("feature boom")
		})},
	}); err == nil {
		t.Fatalf("feature install error was swallowed")
	}
	if _, err := NewRuntime(Config{
		Source:         "1",
		OnRuntimeReady: func(*Runtime) error { return errors.New("ready boom") },
	}); err == nil {
		t.Fatalf("OnRuntimeReady error was swallowed")
	}
	if _, err := NewRuntime(Config{
		Source:    "1",
		Functions: map[string]any{"bad": 1},
	}); err == nil {
		t.Fatalf("invalid Functions entry was accepted")
	}
}

func TestCoverageFeaturesAndEnv(t *testing.T) {
	var sawCleanup bool
	feat := FeatureFunc(func(env *Env) (func() error, error) {
		if env.Runtime() == nil || env.Raw() == nil || env.Loop() == nil {
			t.Errorf("feature environment is incomplete")
		}
		if env.Config().Name == "" {
			t.Errorf("feature config name is empty")
		}
		if err := env.Set("featureValue", 7); err != nil {
			return nil, err
		}
		if err := env.SetFunc("featureAdd", func(a, b int) int { return a + b }); err != nil {
			return nil, err
		}
		if err := env.SetNamespace("featureNS", map[string]any{"x": 1}); err != nil {
			return nil, err
		}
		return func() error {
			sawCleanup = true
			return nil
		}, nil
	})
	rt, err := NewRuntime(Config{
		Source:   `export function read() { return featureValue + featureAdd(1, 2) + featureNS.x; }`,
		Features: []Feature{feat},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	got, err := rt.Call(context.Background(), "read")
	if err != nil || got.String() != "11" {
		t.Fatalf("read = %v, %v", got, err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !sawCleanup {
		t.Fatalf("feature cleanup did not run")
	}
}

func TestCoverageCloseErrors(t *testing.T) {
	rt, err := NewRuntime(Config{
		Source:         "1",
		OnRuntimeClose: func(*Runtime) error { return errors.New("close boom") },
		Features: []Feature{FeatureFunc(func(*Env) (func() error, error) {
			return func() error { return errors.New("cleanup boom") }, nil
		})},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	err = rt.Close()
	if err == nil || !strings.Contains(err.Error(), "close boom") || !strings.Contains(err.Error(), "cleanup boom") {
		t.Fatalf("Close errors = %v", err)
	}
}

func TestCoveragePool(t *testing.T) {
	ex := covExec(t, Config{Source: "export function f() { return 1; }", PoolSize: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ex.pool.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire(canceled) = %v", err)
	}
	ex.pool.maxIdle = 0
	if err := ex.Run(context.Background(), func(*Runtime) error { return nil }); err != nil {
		t.Fatalf("Run with maxIdle 0: %v", err)
	}
	ex.pool.close()
	ex.pool.close() // idempotent
	if _, err := ex.pool.acquire(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("acquire after close = %v", err)
	}

	ex2 := covExec(t, Config{
		Source:         `export function spin() { for (;;) {} }`,
		DefaultTimeout: 10 * time.Millisecond,
		PoolSize:       1,
	})
	if _, err := ex2.Call(context.Background(), "spin"); !IsTimeout(err) {
		t.Fatalf("timeout error = %v", err)
	}
	// The broken runtime is discarded and the next call gets a fresh one.
	if _, err := ex2.Call(context.Background(), "spin"); err == nil {
		t.Fatalf("second call unexpectedly succeeded")
	}
}

func TestCoverageScriptAndTypeScriptModes(t *testing.T) {
	rt := covRuntime(t, Config{
		Name:   "classic.js",
		Mode:   ModeScript,
		Source: `function twice(x) { return x * 2; } var answer = twice(21);`,
	})
	if got, err := rt.Call(context.Background(), "twice", 3); err != nil || got.String() != "6" {
		t.Fatalf("script twice = %v, %v", got, err)
	}
	if rt.Exports() != nil {
		t.Fatalf("script Exports = %v", rt.Exports())
	}

	ts := covRuntime(t, Config{
		Name:       "typed.js",
		TypeScript: true,
		Source:     `export function twice(x: number): number { return x * 2; }`,
	})
	if got, err := ts.Call(context.Background(), "twice", 4); err != nil || got.String() != "8" {
		t.Fatalf("TypeScript twice = %v, %v", got, err)
	}

	mod := covRuntime(t, Config{
		Name:   "module.js",
		Mode:   ModeModule,
		Source: `export const n = 3; export function get() { return n; }`,
	})
	if got, err := mod.Call(context.Background(), "get"); err != nil || got.String() != "3" {
		t.Fatalf("module get = %v, %v", got, err)
	}
}

func TestCoverageParamConversions(t *testing.T) {
	type point struct {
		X int `json:"x"`
	}
	rt := covRuntime(t, Config{
		Source: `export function ok() {
  return [host.map({ a: 1, b: 2 }), host.slice(["a", "b"]), host.ptr(3), host.any("x"), host.struct({ x: 4 })];
}
export function bad() {
  try { host.bad("no"); return "no error"; } catch (e) { return e.message; }
}`,
	})
	if err := rt.SetNamespace("host", map[string]any{
		"map":    func(m map[string]int) int { return m["a"] + m["b"] },
		"slice":  func(s []string) int { return len(s) },
		"ptr":    func(p *int) int { return *p },
		"any":    func(v any) string { return fmt.Sprint(v) },
		"struct": func(p point) int { return p.X },
		"bad":    func(n int) int { return n },
	}); err != nil {
		t.Fatalf("SetNamespace: %v", err)
	}
	got, err := rt.Call(context.Background(), "ok")
	if err != nil {
		t.Fatalf("ok: %v", err)
	}
	if text := fmt.Sprint(mustToGo(t, got)); text != "[3 2 3 x 4]" {
		t.Fatalf("ok = %s", text)
	}
	got, err = rt.Call(context.Background(), "bad")
	if err != nil {
		t.Fatalf("bad: %v", err)
	}
	if text := got.String(); !strings.Contains(text, "cannot convert") {
		t.Fatalf("bad message = %q", text)
	}
}

func mustToGo(t *testing.T, v *Value) any {
	t.Helper()
	g, err := v.ToGo()
	if err != nil {
		t.Fatalf("ToGo: %v", err)
	}
	return g
}

func TestCoverageRejectionReasons(t *testing.T) {
	rt := covRuntime(t, Config{
		Source: `export function undef() { return Promise.reject(undefined); }
export function nul() { return Promise.reject(null); }
export function text() { return Promise.reject("boom"); }
export function object() { return Promise.reject({}); }
export function named() { return Promise.reject(new RangeError("nope")); }`,
	})
	cases := map[string]string{
		"undef":  "promise rejected with undefined",
		"nul":    "promise rejected with null",
		"text":   "boom",
		"object": "[object Object]",
		"named":  "nope",
	}
	for fn, want := range cases {
		_, err := rt.Call(context.Background(), fn)
		if err == nil {
			t.Fatalf("%s returned no error", fn)
		}
		var jsErr *JSError
		if !errors.As(err, &jsErr) {
			t.Fatalf("%s error = %T", fn, err)
		}
		if jsErr.Message != want {
			t.Fatalf("%s message = %q, want %q", fn, jsErr.Message, want)
		}
		if fn == "named" && (jsErr.Name != "RangeError" || jsErr.Stack == "") {
			t.Fatalf("named error = %+v", jsErr)
		}
	}
}

func TestCoverageRuntimeEdges(t *testing.T) {
	rt := covRuntime(t, Config{Source: `export function id(x) { return x; }`})
	ctx := context.Background()
	if _, err := rt.Call(ctx, "missing"); !errors.Is(err, ErrNotFunction) {
		t.Fatalf("Call(missing) = %v", err)
	}
	if _, err := rt.Call(ctx, "id", make(chan int)); err == nil {
		t.Fatalf("unconvertible argument accepted")
	}
	other := covRuntime(t, Config{Source: "export function id(x) { return x; }"})
	foreign, _ := other.Get("id")
	if _, err := rt.CallValue(ctx, foreign); err == nil {
		t.Fatalf("CallValue(foreign) accepted")
	}
	if rt.currentContext() != context.Background() {
		t.Fatalf("currentContext outside a call is not Background")
	}
	if _, err := rt.Eval(ctx, "syntax error ("); err == nil {
		t.Fatalf("bad Eval source accepted")
	}

	if err := rt.SetFunc("evalFromHost", func() string {
		v, err := rt.Eval(ctx, "1 + 1")
		if err != nil {
			return err.Error()
		}
		return v.String()
	}); err != nil {
		t.Fatalf("SetFunc evalFromHost: %v", err)
	}
	if got, err := rt.Eval(ctx, "evalFromHost()"); err != nil || got.String() != "2" {
		t.Fatalf("evalFromHost = %v, %v", got, err)
	}
	if err := rt.SetFunc("callFromHost", func() string {
		v, err := rt.Call(ctx, "id", 5)
		if err != nil {
			return err.Error()
		}
		return v.String()
	}); err != nil {
		t.Fatalf("SetFunc callFromHost: %v", err)
	}
	if got, err := rt.Eval(ctx, "callFromHost()"); err != nil || got.String() != "5" {
		t.Fatalf("callFromHost = %v, %v", got, err)
	}

	num, _ := rt.Eval(ctx, "1")
	if err := num.Set("x", 1); err == nil {
		t.Fatalf("Value.Set on a number accepted")
	}
	object, _ := rt.Eval(ctx, "({})")
	if err := object.Set("x", foreign); err == nil {
		t.Fatalf("Value.Set(foreign) accepted")
	}
}

func TestCoverageCompileTimeoutAndErrors(t *testing.T) {
	if _, err := New(Config{Source: "1", CompileTimeout: time.Second}); err != nil {
		t.Fatalf("CompileTimeout: %v", err)
	}
	if _, err := NewRuntime(Config{Source: "syntax error ("}); err == nil {
		t.Fatalf("bad source accepted")
	}
	if _, err := NewRuntime(Config{}); !errors.Is(err, ErrNoSource) {
		t.Fatalf("empty config = %v", err)
	}
}

func TestCoverageOnLoopPanic(t *testing.T) {
	rt := covRuntime(t, Config{Source: "1"})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatalf("onLoop did not re-panic")
			}
		}()
		_ = rt.onLoop(context.Background(), "panic", func(context.Context) error { panic("boom") })
	}()
	if !rt.broken.Load() {
		t.Fatalf("runtime not marked broken after panic")
	}
}

func TestCoverageInstallWebAssemblyErrors(t *testing.T) {
	rt := covRuntime(t, Config{Source: "1"})
	env := &Env{r: rt, cfg: rt.cfg}
	if cleanup, err := installWebAssembly(env, WebAssemblyOptions{Disable: true}); err != nil || cleanup != nil {
		t.Fatalf("disabled install returned cleanup=%v, err=%v", cleanup != nil, err)
	}
	if _, err := installWebAssembly(env, WebAssemblyOptions{MemoryLimitPages: 65537}); err == nil {
		t.Fatalf("oversized MemoryLimitPages accepted")
	}
}

func TestCoverageMiscBranches(t *testing.T) {
	// consoleFeature with no writer is a no-op.
	if cleanup, err := (consoleFeature{}).Install(nil); err != nil || cleanup != nil {
		t.Fatalf("consoleFeature nil returned cleanup=%v, err=%v", cleanup != nil, err)
	}

	rt := covRuntime(t, Config{Source: `export function id(x) { return x; } export const obj = { n: 1 };`})
	ctx := context.Background()
	if _, err := rt.Eval(ctx, "1"); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	// Float errors for non-numbers.
	for _, expr := range []string{"'x'", "9n", "Symbol('x')"} {
		v, err := rt.Eval(ctx, expr)
		if err != nil {
			t.Fatalf("Eval %s: %v", expr, err)
		}
		if _, err := v.Float(); err == nil {
			t.Fatalf("Float(%s) accepted", expr)
		}
	}
	// Into target errors.
	if err := rt.CallInto(ctx, "id", 1, 2); err == nil {
		t.Fatalf("CallInto(non-pointer) accepted")
	}
	if err := rt.EvalInto(ctx, "1 + 1", 1); err == nil {
		t.Fatalf("EvalInto(non-pointer) accepted")
	}
	if err := rt.CallValueInto(ctx, nil, new(int)); err == nil {
		t.Fatalf("CallValueInto(nil) accepted")
	}
	if _, err := rt.Export("id"); err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Methods on a closed runtime.
	closed := covRuntime(t, Config{Source: "export const x = 1;"})
	v, _ := closed.Get("x")
	if err := closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := v.ToGo(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Value.ToGo after Close = %v", err)
	}
	if _, err := closed.Export("x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Export after Close = %v", err)
	}
	if _, err := closed.Func("x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Func after Close = %v", err)
	}

	// Nil executor close.
	var nilExec *Executor
	if err := nilExec.Close(); err != nil {
		t.Fatalf("nil Executor.Close = %v", err)
	}

	// A pool with an invalid size still gets one slot.
	pool := newRuntimePool(Config{}, rt.prog)
	if cap(pool.sem) != 1 {
		t.Fatalf("newRuntimePool size = %d", cap(pool.sem))
	}
	pool.close()

	// execContext honors an existing deadline and handles a nil context.
	var nilContext context.Context
	if ctx, cancel := rt.execContext(nilContext); ctx == nil || cancel == nil {
		t.Fatalf("execContext(nil) failed")
	} else {
		cancel()
	}
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), time.Second)
	defer deadlineCancel()
	if got, cancel := rt.execContext(deadlineCtx); got != deadlineCtx {
		t.Fatalf("execContext replaced a context that already has a deadline")
	} else {
		cancel()
	}

	// IsTimeout/IsCanceled handle interrupts without a cause.
	empty := &moejs.InterruptedError{}
	if IsTimeout(empty) || IsCanceled(empty) {
		t.Fatalf("empty interrupt classified as timeout/cancel")
	}

	// fromGo rejects a foreign engine function value.
	other := covRuntime(t, Config{Source: "export function id(x) { return x; }"})
	foreignFn, _ := other.Get("id")
	if _, err := rt.fromGo(foreignFn.Raw()); err == nil {
		t.Fatalf("fromGo(foreign function) accepted")
	}
	// A named byte slice is not the []byte fast path.
	type namedBytes []byte
	if v, err := rt.fromGo(namedBytes{1, 2}); err != nil || !v.IsObject() {
		t.Fatalf("fromGo(named []byte) = %v, %v", v, err)
	}
	// Non-string map keys fall through to moejs, which may convert them.
	_, _ = rt.fromGo(map[int]int{1: 2})
	// An error while converting a map member aborts the map.
	if _, err := rt.fromGoMap(map[string]any{"bad": foreignFn}); err == nil {
		t.Fatalf("fromGoMap(foreign) accepted")
	}

	// HostFS with a missing root fails to install.
	if _, err := NewRuntime(Config{
		Source: "1",
		FS:     &HostFS{Root: filepath.Join(t.TempDir(), "missing")},
	}); err == nil {
		t.Fatalf("HostFS with a missing root was accepted")
	}
}

func TestCoverageExecutorErrorPaths(t *testing.T) {
	ex := covExec(t, Config{Source: `export const obj = { n: 1 }; export function id(x) { return x; }`})
	ctx := context.Background()
	if err := ex.GetInto(ctx, "obj", 1); err == nil {
		t.Fatalf("GetInto(non-pointer) accepted")
	}
	if err := ex.CallInto(ctx, "missing", new(int)); !errors.Is(err, ErrNotFunction) {
		t.Fatalf("CallInto(missing) = %v", err)
	}
	if err := ex.EvalInto(ctx, "syntax error (", new(int)); err == nil {
		t.Fatalf("EvalInto(bad source) accepted")
	}

	rt := covRuntime(t, Config{
		Source: `const v = await Promise.resolve(3);
export function get() { return v; }`,
	})
	if got, err := rt.Call(ctx, "get"); err != nil || got.String() != "3" {
		t.Fatalf("top-level await = %v, %v", got, err)
	}
}

func TestCoverageUnhandledRejectionCallback(t *testing.T) {
	seen := make(chan *JSError, 1)
	rt := covRuntime(t, Config{
		Source: `export function fire() { Promise.reject(new Error("unhandled")); return 1; }`,
		OnUnhandledRejection: func(err *JSError) {
			select {
			case seen <- err:
			default:
			}
		},
	})
	if _, err := rt.Call(context.Background(), "fire"); err != nil {
		t.Fatalf("fire: %v", err)
	}
	select {
	case err := <-seen:
		if err.Message != "unhandled" {
			t.Fatalf("rejection = %+v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("OnUnhandledRejection was not called")
	}
}

func TestCoverageFromGoEdges(t *testing.T) {
	rt := covRuntime(t, Config{Source: "1"})
	var nf moejs.NativeFunc = func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
		return moejs.Undefined(), nil
	}
	if _, err := rt.fromGo(nf); err != nil {
		t.Fatalf("fromGo(NativeFunc): %v", err)
	}
	if v, err := rt.fromGo(rt.raw.Realm().NewObject()); err != nil || !v.IsObject() {
		t.Fatalf("fromGo(*Object) = %v, %v", v, err)
	}
	type namedMap map[string]int
	if _, err := rt.fromGo(namedMap(nil)); err != nil {
		t.Fatalf("fromGo(nil named map): %v", err)
	}
	type namedSlice []int
	if _, err := rt.fromGo(namedSlice(nil)); err != nil {
		t.Fatalf("fromGo(nil named slice): %v", err)
	}
	if _, err := rt.fromGo(func() (int, int, int) { return 1, 2, 3 }); err == nil {
		t.Fatalf("fromGo(bad func signature) accepted")
	}
}
