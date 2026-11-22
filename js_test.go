package js_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	js "github.com/malivvan/js"
)

func newExec(t *testing.T, cfg js.Config) *js.Executor {
	t.Helper()
	ex, err := js.New(cfg)
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

func newRuntime(t *testing.T, cfg js.Config) *js.Runtime {
	t.Helper()
	rt, err := js.NewRuntime(cfg)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return rt
}

type task struct {
	Prompt string `json:"prompt"`
	N      int    `json:"n"`
}

type built struct {
	Prompt string `json:"prompt"`
	N      int    `json:"n"`
}

func TestExposeGlobalsAndCall(t *testing.T) {
	ex := newExec(t, js.Config{
		Name: "greet.js",
		Source: `
export function greet(name) {
  return utils.prefix + " " + name.trim() + " (" + utils.add(2, 3) + ")";
}`,
		Globals: map[string]any{
			"utils": map[string]any{
				"prefix": "hello",
				"add":    func(a, b int) int { return a + b },
			},
		},
	})

	got, err := ex.Call(context.Background(), "greet", "  world  ")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "hello world (5)" {
		t.Fatalf("got %#v, want %q", got, "hello world (5)")
	}
}

func TestCallIntoStruct(t *testing.T) {
	ex := newExec(t, js.Config{
		Name: "build.js",
		Source: `
export function build(task) {
  return { prompt: task.prompt.trim(), n: (task.n ?? 1) + 1 };
}`,
	})

	var out built
	err := ex.CallInto(context.Background(), "build", &out, task{Prompt: "  cat ", N: 2})
	if err != nil {
		t.Fatalf("CallInto: %v", err)
	}
	if out.Prompt != "cat" || out.N != 3 {
		t.Fatalf("got %+v", out)
	}
}

func TestGoFunctionSignatures(t *testing.T) {
	type ctxKey string
	host := map[string]any{
		"sum": func(nums ...int) int {
			n := 0
			for _, x := range nums {
				n += x
			}
			return n
		},
		"pair": func(a, b int) (int, error) {
			if b == 0 {
				return 0, errors.New("division by zero")
			}
			return a / b, nil
		},
		"fail": func() error { return errors.New("nope") },
		"withCtx": func(ctx context.Context) string {
			v, _ := ctx.Value(ctxKey("k")).(string)
			return "ctx:" + v
		},
		"echo": func(task task) built {
			return built{Prompt: task.Prompt, N: task.N}
		},
	}

	ex := newExec(t, js.Config{
		Name: "host.js",
		Source: `
export function calc() { return host.sum(1, 2, 3); }
export function divide(a, b) { return host.pair(a, b); }
export function caught() { try { host.fail(); } catch (e) { return e.message; } }
export function contextValue() { return host.withCtx(); }
export function echo(t) { return host.echo(t); }`,
		Globals: map[string]any{"host": host},
	})

	ctx := context.WithValue(context.Background(), ctxKey("k"), "hello")

	if got, err := ex.Call(ctx, "calc"); err != nil || got != int64(6) {
		t.Fatalf("calc: got %#v, err %v", got, err)
	}
	if got, err := ex.Call(ctx, "divide", 9, 3); err != nil || got != int64(3) {
		t.Fatalf("divide: got %#v, err %v", got, err)
	}
	if got, err := ex.Call(ctx, "divide", 9, 0); err == nil {
		t.Fatalf("divide by zero: got %#v, want error", got)
	}
	if got, err := ex.Call(ctx, "caught"); err != nil || got != "nope" {
		t.Fatalf("caught: got %#v, err %v", got, err)
	}
	if got, err := ex.Call(ctx, "contextValue"); err != nil || got != "ctx:hello" {
		t.Fatalf("contextValue: got %#v, err %v", got, err)
	}

	var echo built
	if err := ex.CallInto(ctx, "echo", &echo, task{Prompt: "hi", N: 7}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if echo.Prompt != "hi" || echo.N != 7 {
		t.Fatalf("echo: got %+v", echo)
	}
}

func TestRuntimeAccessAndValues(t *testing.T) {
	rt := newRuntime(t, js.Config{
		Name: "state.js",
		Source: `
export const config = { retries: 3, tags: ["a", "b"] };
export let counter = 0;
export function bump() { return ++counter; }
export function makeAdder(n) { return (x) => x + n; }
export function waitFor(v) { return Promise.resolve(v); }`,
	})
	ctx := context.Background()

	retries, err := rt.Get("config.retries")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n, err := retries.Int(); err != nil || n != 3 {
		t.Fatalf("retries: %d, %v", n, err)
	}

	tags, err := rt.Get("config.tags")
	if err != nil {
		t.Fatalf("Get tags: %v", err)
	}
	first, err := tags.Get("0")
	if err != nil {
		t.Fatalf("Get 0: %v", err)
	}
	if first.String() != "a" {
		t.Fatalf("tags[0] = %q", first.String())
	}
	if !tags.IsArray() || tags.Type() != "object" {
		t.Fatalf("tags type: array=%v type=%q", tags.IsArray(), tags.Type())
	}

	funcValue, err := rt.Func("makeAdder")
	if err != nil {
		t.Fatalf("Func: %v", err)
	}
	adder, err := funcValue.Call(ctx, 5)
	if err != nil {
		t.Fatalf("Call makeAdder: %v", err)
	}
	if !adder.IsFunction() {
		t.Fatalf("makeAdder result is not a function")
	}
	sum, err := adder.Call(ctx, 7)
	if err != nil {
		t.Fatalf("Call adder: %v", err)
	}
	if n, err := sum.Int(); err != nil || n != 12 {
		t.Fatalf("adder(7) = %d, %v", n, err)
	}

	if err := rt.Set("extra", map[string]any{"x": 1}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := rt.SetPath("nested.a.b", 5); err != nil {
		t.Fatalf("SetPath: %v", err)
	}
	if got, err := rt.Get("nested.a.b"); err != nil {
		t.Fatalf("Get nested: %v", err)
	} else if n, _ := got.Int(); n != 5 {
		t.Fatalf("nested.a.b = %d", n)
	}

	if _, err := rt.Call(ctx, "bump"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	counter, err := rt.Export("counter")
	if err != nil {
		t.Fatalf("Export counter: %v", err)
	}
	if n, _ := counter.Int(); n != 1 {
		t.Fatalf("counter = %d", n)
	}
	if _, err := rt.Export("missing"); !errors.Is(err, js.ErrNotFound) {
		t.Fatalf("missing export error = %v", err)
	}

	awaited, err := rt.Call(ctx, "waitFor", "ok")
	if err != nil {
		t.Fatalf("waitFor: %v", err)
	}
	if awaited.String() != "ok" {
		t.Fatalf("waitFor = %q", awaited.String())
	}

	if err := rt.SetFunc("add", func(a, b int) int { return a + b }); err != nil {
		t.Fatalf("SetFunc: %v", err)
	}
	if got, err := rt.Eval(ctx, "add(20, 22)"); err != nil {
		t.Fatalf("Eval: %v", err)
	} else if n, _ := got.Int(); n != 42 {
		t.Fatalf("add = %d", n)
	}

	if got, err := rt.Eval(ctx, "({ answer: 42 })"); err != nil {
		t.Fatalf("Eval object: %v", err)
	} else {
		var out struct {
			Answer int `json:"answer"`
		}
		if err := got.Into(&out); err != nil || out.Answer != 42 {
			t.Fatalf("Into: %+v, %v", out, err)
		}
	}
}

func TestScriptMode(t *testing.T) {
	rt := newRuntime(t, js.Config{
		Name:   "script.js",
		Mode:   js.ModeScript,
		Source: `function twice(x) { return x * 2 } var answer = twice(21);`,
	})
	ctx := context.Background()
	if got, err := rt.Call(ctx, "twice", 4); err != nil {
		t.Fatalf("twice: %v", err)
	} else if n, _ := got.Int(); n != 8 {
		t.Fatalf("twice = %d", n)
	}
	if got, err := rt.Eval(ctx, "answer"); err != nil {
		t.Fatalf("Eval: %v", err)
	} else if n, _ := got.Int(); n != 42 {
		t.Fatalf("answer = %d", n)
	}
}

func TestAsyncTimersAndRejection(t *testing.T) {
	ex := newExec(t, js.Config{
		Name: "async.js",
		Source: `
export async function later(ms) {
  await new Promise(r => setTimeout(r, ms));
  return "done";
}
export async function fail() {
  await null;
  throw new RangeError("nope");
}
export function bad() { throw new TypeError("bad input"); }`,
	})
	ctx := context.Background()

	start := time.Now()
	got, err := ex.Call(ctx, "later", 2)
	if err != nil {
		t.Fatalf("later: %v", err)
	}
	if got != "done" {
		t.Fatalf("later = %#v", got)
	}
	if time.Since(start) < time.Millisecond {
		t.Fatalf("later returned too soon")
	}

	_, err = ex.Call(ctx, "fail")
	if err == nil {
		t.Fatalf("fail returned no error")
	}
	var jsErr *js.JSError
	if !errors.As(err, &jsErr) {
		t.Fatalf("fail error is %T, want *JSError: %v", err, err)
	}
	if jsErr.Name != "RangeError" || jsErr.Message != "nope" {
		t.Fatalf("JS error = %+v", jsErr)
	}

	_, err = ex.Call(ctx, "bad")
	if !js.IsException(err) {
		t.Fatalf("bad error is %T, want a JS exception: %v", err, err)
	}
	exc, _ := js.AsException(err)
	if exc.Name() != "TypeError" || exc.Message() != "bad input" {
		t.Fatalf("exception = %s: %s", exc.Name(), exc.Message())
	}
	if js.JSStack(err) == "" {
		t.Fatalf("expected a stack trace")
	}
}

func TestUnhandledRejection(t *testing.T) {
	var mu sync.Mutex
	var seen []*js.JSError
	ex := newExec(t, js.Config{
		Name: "reject.js",
		Source: `
export function fire() { Promise.reject(new TypeError("unhandled")); return 1; }`,
		OnUnhandledRejection: func(err *js.JSError) {
			mu.Lock()
			seen = append(seen, err)
			mu.Unlock()
		},
	})
	if _, err := ex.Call(context.Background(), "fire"); err != nil {
		t.Fatalf("fire: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("saw %d unhandled rejections, want 1", len(seen))
	}
	if seen[0].Name != "TypeError" || seen[0].Message != "unhandled" {
		t.Fatalf("rejection = %+v", seen[0])
	}
}

func TestTimeoutAndMemoryLimit(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		ex := newExec(t, js.Config{
			Name:   "spin.js",
			Source: `export function spin() { for (;;) {} }`,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		_, err := ex.Call(ctx, "spin")
		if err == nil {
			t.Fatalf("spin returned no error")
		}
		if !js.IsTimeout(err) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout error = %v", err)
		}
	})

	t.Run("memory", func(t *testing.T) {
		ex := newExec(t, js.Config{
			Name:        "mem.js",
			MemoryLimit: 1 << 20,
			Source: `
export function blow() {
  const a = [];
  for (;;) { a.push("x".repeat(65536)); }
}`,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := ex.Call(ctx, "blow")
		if err == nil {
			t.Fatalf("blow returned no error")
		}
		if !js.IsMemoryLimit(err) {
			t.Fatalf("memory error = %v", err)
		}
	})
}

func TestModulesFromFS(t *testing.T) {
	main := `
import { double } from "./util.js";
export function run(x) { return double(x); }
export async function dynamic(x) {
  const m = await import("./util.js");
  return m.double(x) + 1;
}`
	util := `export function double(x) { return x * 2; }`
	fsys := fstest.MapFS{
		"src/main.js": {Data: []byte(main)},
		"src/util.js": {Data: []byte(util)},
	}
	ex := newExec(t, js.Config{
		Name:       "main.js",
		Source:     main,
		ModuleFS:   fsys,
		ModuleRoot: "src",
	})
	ctx := context.Background()
	if got, err := ex.Call(ctx, "run", 21); err != nil {
		t.Fatalf("run: %v", err)
	} else if got != int64(42) {
		t.Fatalf("run = %#v", got)
	}
	if got, err := ex.Call(ctx, "dynamic", 20); err != nil {
		t.Fatalf("dynamic: %v", err)
	} else if got != int64(41) {
		t.Fatalf("dynamic = %#v", got)
	}
}

func TestModulesFromMap(t *testing.T) {
	ex := newExec(t, js.Config{
		Name: "main.js",
		Source: `
import { suffix } from "./strings.js";
export function decorate(s) { return s + suffix; }`,
		Modules: map[string]string{
			"./strings.js": `export const suffix = "!";`,
		},
	})
	got, err := ex.Call(context.Background(), "decorate", "hi")
	if err != nil {
		t.Fatalf("decorate: %v", err)
	}
	if got != "hi!" {
		t.Fatalf("decorate = %#v", got)
	}
}

func TestHostFS(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello fs"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := `
import { readFile, writeFile } from "node:fs/promises";
export async function read() {
  return (await readFile("/workspace/hello.txt", "utf8")).trim();
}
export async function write() {
  await writeFile("/workspace/out.txt", "written");
  return "ok";
}`
	ex := newExec(t, js.Config{
		Name:    "fs.js",
		Source:  source,
		FS:      &js.HostFS{Root: dir, Mount: "/workspace"},
		Network: js.NoNetwork{},
	})
	ctx := context.Background()
	got, err := ex.Call(ctx, "read")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "hello fs" {
		t.Fatalf("read = %#v", got)
	}
	if _, err := ex.Call(ctx, "write"); err == nil {
		t.Fatalf("write succeeded on a read-only file system")
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("out.txt should not exist: %v", err)
	}
}

func TestHostFSWrite(t *testing.T) {
	dir := t.TempDir()
	ex := newExec(t, js.Config{
		Name:   "fs.js",
		Source: `import { writeFile } from "node:fs/promises"; export async function write() { await writeFile("/workspace/out.txt", "written"); return "ok"; }`,
		FS:     &js.HostFS{Root: dir, Mount: "/workspace", Write: true},
	})
	if _, err := ex.Call(context.Background(), "write"); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "written" {
		t.Fatalf("out.txt = %q", data)
	}
}

func TestHostNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, " network body \n")
	}))
	defer srv.Close()
	url := srv.URL
	host := strings.TrimPrefix(srv.URL, "http://")

	source := fmt.Sprintf(`
export async function get() {
  const r = await fetch(%q);
  return (await r.text()).trim();
}`, url)

	ex := newExec(t, js.Config{
		Name:    "net.js",
		Source:  source,
		Network: &js.HostNetwork{Client: srv.Client(), Allow: js.AllowHosts(host)},
	})
	got, err := ex.Call(context.Background(), "get")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "network body" {
		t.Fatalf("get = %#v", got)
	}

	blocked := newExec(t, js.Config{
		Name:    "net.js",
		Source:  source,
		Network: &js.HostNetwork{Client: srv.Client(), Allow: js.AllowHosts("example.com")},
	})
	if _, err := blocked.Call(context.Background(), "get"); err == nil {
		t.Fatalf("blocked fetch succeeded")
	}
}

func TestConcurrentExecutor(t *testing.T) {
	var ready atomic.Int64
	ex := newExec(t, js.Config{
		Name:     "add.js",
		Source:   `export function add(a, b) { return a + b; }`,
		PoolSize: 4,
		MaxIdle:  4,
		OnRuntimeReady: func(*js.Runtime) error {
			ready.Add(1)
			return nil
		},
	})
	ctx := context.Background()
	const n = 64
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := ex.Call(ctx, "add", i, 1)
			if err != nil {
				errs <- err
				return
			}
			if got != int64(i+1) {
				errs <- fmt.Errorf("add(%d,1) = %#v", i, got)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if ready.Load() < 1 || ready.Load() > 4 {
		t.Fatalf("created %d runtimes, want 1..4", ready.Load())
	}
}

func TestNoReuse(t *testing.T) {
	var created atomic.Int64
	ex := newExec(t, js.Config{
		Name:    "add.js",
		Source:  `export function add(a, b) { return a + b; }`,
		NoReuse: true,
		OnRuntimeReady: func(*js.Runtime) error {
			created.Add(1)
			return nil
		},
	})
	for i := 0; i < 3; i++ {
		if _, err := ex.Call(context.Background(), "add", i, 1); err != nil {
			t.Fatal(err)
		}
	}
	if created.Load() != 3 {
		t.Fatalf("created %d runtimes, want 3", created.Load())
	}
}

func TestTypeScript(t *testing.T) {
	ex := newExec(t, js.Config{
		Name: "add.ts",
		Source: `
export function add(a: number, b: number): number { return a + b; }
export function greet(name: string): string { return ` + "`hi ${name}`" + `; }`,
	})
	got, err := ex.Call(context.Background(), "add", 2, 3)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if got != int64(5) {
		t.Fatalf("add = %#v", got)
	}
	if got, err := ex.Call(context.Background(), "greet", "bob"); err != nil || got != "hi bob" {
		t.Fatalf("greet = %#v, %v", got, err)
	}
}

func TestConsole(t *testing.T) {
	var buf bytes.Buffer
	ex := newExec(t, js.Config{
		Name:    "log.js",
		Source:  `export function log() { console.log("hello", 2, {a: 3}); console.error("bad"); return 1; }`,
		Console: &buf,
	})
	if _, err := ex.Call(context.Background(), "log"); err != nil {
		t.Fatalf("log: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "hello 2 {\"a\":3}\n") || !strings.Contains(got, "bad\n") {
		t.Fatalf("console output = %q", got)
	}
}

func TestWebAssemblyBuildGating(t *testing.T) {
	ex := newExec(t, js.Config{
		Name:   "wasm-gate.js",
		Source: `export function type() { return typeof WebAssembly; }`,
	})
	got, err := ex.Call(context.Background(), "type")
	if err != nil {
		t.Fatalf("type: %v", err)
	}
	want := "undefined"
	if js.WebAssemblySupported {
		want = "object"
	}
	if got != want {
		t.Fatalf("typeof WebAssembly = %#v, want %q", got, want)
	}
	if !js.WebAssemblySupported {
		if _, err := js.NewRuntime(js.Config{
			Source:      "1",
			WebAssembly: js.WebAssemblyOptions{MemoryLimitPages: 1},
		}); err == nil {
			t.Fatalf("memory options without the canopy_wasm tag were accepted")
		}
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := js.New(js.Config{}); !errors.Is(err, js.ErrNoSource) {
		t.Fatalf("empty config error = %v", err)
	}
	if _, err := js.New(js.Config{Source: "1", SourceFile: "x.js"}); err == nil {
		t.Fatalf("source and source file both set: no error")
	}
	if _, err := js.New(js.Config{Source: "1", Mode: 99}); err == nil {
		t.Fatalf("invalid mode: no error")
	}
}

func TestFeatureComposition(t *testing.T) {
	// A user-written feature can add globals and clean up.
	var closed atomic.Bool
	feature := js.FeatureFunc(func(env *js.Env) (func() error, error) {
		if err := env.SetFunc("featureValue", func() int { return 7 }); err != nil {
			return nil, err
		}
		return func() error {
			closed.Store(true)
			return nil
		}, nil
	})
	ex := newExec(t, js.Config{
		Name:     "feature.js",
		Source:   `export function value() { return featureValue(); }`,
		Features: []js.Feature{feature},
		NoReuse:  true,
	})
	got, err := ex.Call(context.Background(), "value")
	if err != nil {
		t.Fatalf("value: %v", err)
	}
	if got != int64(7) {
		t.Fatalf("value = %#v", got)
	}
	if !closed.Load() {
		t.Fatalf("feature cleanup did not run")
	}
}

func TestNoFS(t *testing.T) {
	_, err := js.New(js.Config{
		Name:   "fs.js",
		Source: `import { readFile } from "node:fs/promises"; export function read() { return readFile.length; }`,
		FS:     js.NoFS{},
	})
	if err == nil {
		t.Fatalf("importing node:fs without FS succeeded")
	}
	if !errors.Is(err, js.ErrModuleNotFound) {
		t.Fatalf("error = %v, want ErrModuleNotFound", err)
	}
}

func TestPanicBecomesError(t *testing.T) {
	ex := newExec(t, js.Config{
		Name: "panic.js",
		Source: `export function run() {
  try { host.boom(); } catch (e) { return e.message; }
}`,
		Globals: map[string]any{
			"host": map[string]any{
				"boom": func() { panic("kaboom") },
			},
		},
	})
	got, err := ex.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(got.(string), "kaboom") {
		t.Fatalf("panic message = %#v", got)
	}
}
