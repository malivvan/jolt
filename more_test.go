package jolt_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/malivvan/jolt"
)

func TestDecodeLimits(t *testing.T) {
	ex := newExec(t, jolt.Config{
		Name:   "decode.js",
		Source: `export function big() { return "x".repeat(1024); }`,
		Decode: jolt.DecodeOptions{MaxBytes: 32},
	})
	if _, err := ex.Call(context.Background(), "big"); !errors.Is(err, jolt.ErrTooLarge) {
		t.Fatalf("decode error = %v, want ErrTooLarge", err)
	}
}

func TestDefaultTimeout(t *testing.T) {
	ex := newExec(t, jolt.Config{
		Name:           "spin.js",
		Source:         `export function spin() { for (;;) {} }`,
		DefaultTimeout: 25 * time.Millisecond,
	})
	_, err := ex.Call(context.Background(), "spin")
	if !jolt.IsTimeout(err) {
		t.Fatalf("error = %v, want timeout", err)
	}
}

func TestOnLoopError(t *testing.T) {
	seen := make(chan error, 1)
	ex := newExec(t, jolt.Config{
		Name:   "timer.js",
		Source: `export function fire() { setTimeout(() => { throw new Error("timer boom"); }, 0); return "ok"; }`,
		OnLoopError: func(err error) {
			select {
			case seen <- err:
			default:
			}
		},
		PoolSize: 1,
	})
	got, err := ex.Call(context.Background(), "fire")
	if err != nil {
		t.Fatalf("fire: %v", err)
	}
	if got != "ok" {
		t.Fatalf("fire = %#v", got)
	}
	select {
	case err := <-seen:
		if !jolt.IsException(err) {
			t.Fatalf("loop error = %T %v", err, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("OnLoopError was not called")
	}
}

func TestModuleLoader(t *testing.T) {
	loader := jolt.ModuleLoaderFunc(func(referrer, specifier string) (jolt.ModuleSource, error) {
		if specifier == "virtual:math" {
			return jolt.ModuleSource{Name: "virtual:math.js", Source: `export const two = 2;`}, nil
		}
		return jolt.ModuleSource{}, jolt.ErrModuleNotFound
	})
	ex := newExec(t, jolt.Config{
		Name:   "loader.js",
		Source: `import { two } from "virtual:math"; export function run() { return two + 40; }`,
		Loader: loader,
	})
	got, err := ex.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != int64(42) {
		t.Fatalf("run = %#v", got)
	}
}

func TestValueMethods(t *testing.T) {
	rt := newRuntime(t, jolt.Config{
		Name:   "value.js",
		Source: `export const obj = { n: 1.5, ok: true, s: "text", f() { return this.n; } };`,
	})
	ctx := context.Background()
	obj, err := rt.Get("obj")
	if err != nil {
		t.Fatalf("Get obj: %v", err)
	}

	n, err := obj.Get("n")
	if err != nil {
		t.Fatalf("Get n: %v", err)
	}
	if f, err := n.Float(); err != nil || f != 1.5 {
		t.Fatalf("n = %v, %v", f, err)
	}
	ok, err := obj.Get("ok")
	if err != nil || !ok.Bool() {
		t.Fatalf("ok = %v, %v", ok.Bool(), err)
	}
	s, err := obj.Get("s")
	if err != nil {
		t.Fatalf("Get s: %v", err)
	}
	if text, err := s.Text(); err != nil || text != "text" {
		t.Fatalf("s = %q, %v", text, err)
	}

	got, err := obj.CallMethod(ctx, "f")
	if err != nil {
		t.Fatalf("CallMethod: %v", err)
	}
	if f, err := got.Float(); err != nil || f != 1.5 {
		t.Fatalf("f() = %v, %v", f, err)
	}

	if err := obj.Set("n", 2.5); err != nil {
		t.Fatalf("Set n: %v", err)
	}
	if got, err := obj.CallMethod(ctx, "f"); err != nil {
		t.Fatalf("CallMethod after Set: %v", err)
	} else if f, _ := got.Float(); f != 2.5 {
		t.Fatalf("f() after Set = %v", f)
	}
}

func TestAwaitPromiseValue(t *testing.T) {
	rt := newRuntime(t, jolt.Config{
		Name:                 "await.js",
		Source:               `export async function value() { return 7; }`,
		DisableAwaitPromises: true,
	})
	p, err := rt.Call(context.Background(), "value")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !p.IsPromise() {
		t.Fatalf("value() did not return a promise")
	}
	got, err := p.Await(context.Background())
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if n, err := got.Int(); err != nil || n != 7 {
		t.Fatalf("awaited value = %d, %v", n, err)
	}
}

func TestLifecycleHooks(t *testing.T) {
	var ready, closed atomic.Int64
	ex := newExec(t, jolt.Config{
		Name:    "hooks.js",
		Source:  `export function run() { return 1; }`,
		NoReuse: true,
		OnRuntimeReady: func(rt *jolt.Runtime) error {
			if rt.ModuleName() != "hooks.js" {
				t.Errorf("module name = %q", rt.ModuleName())
			}
			ready.Add(1)
			return nil
		},
		OnRuntimeClose: func(*jolt.Runtime) error {
			closed.Add(1)
			return nil
		},
	})
	if _, err := ex.Call(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	if ready.Load() != 1 {
		t.Fatalf("ready = %d", ready.Load())
	}
	if closed.Load() != 1 {
		t.Fatalf("closed = %d", closed.Load())
	}
}

func TestHostFSConcurrent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := newExec(t, jolt.Config{
		Name: "fs.js",
		Source: `import { readFile } from "node:fs/promises";
export async function read() { return (await readFile("/workspace/hello.txt", "utf8")).trim(); }`,
		FS:       &jolt.HostFS{Root: dir, Mount: "/workspace"},
		PoolSize: 4,
		MaxIdle:  4,
	})
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := ex.Call(ctx, "read")
			if err != nil {
				errs <- err
				return
			}
			if got != "hi" {
				errs <- fmt.Errorf("read = %#v", got)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
