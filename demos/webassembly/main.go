//go:build !nowasm && !(js && wasm)

// Command wasm runs a small WebAssembly command module through jolt.
//
// The guest imports wasi_snapshot_preview1.fd_write and prints one line to
// stdout. jolt loads the WASI host module automatically, so the JavaScript
// side passes an empty importObject; WebAssembly.WASI supplies the output
// stream, and RunStart executes the guest's _start during instantiation.
//
// Expected output:
//
//	hello from wasm
//	ok
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/malivvan/jolt"
)

const source = `
export async function run(bytes) {
  await WebAssembly.instantiate(bytes, {});
  return "ok";
}`

func main() {
	exec, err := jolt.New(jolt.Config{
		Name:   "hello-wasi.js",
		Source: source,
		WebAssembly: jolt.WebAssemblyOptions{
			WASI: jolt.WebAssemblyWASIOptions{
				Stdout:   os.Stdout,
				RunStart: true,
			},
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "compile:", err)
		os.Exit(1)
	}
	defer exec.Close()

	out, err := exec.Call(context.Background(), "run", helloWASM())
	if err != nil {
		fmt.Fprintln(os.Stderr, "call:", err)
		os.Exit(1)
	}
	fmt.Println(out)
}

// helloWASM assembles the guest module:
//
//	(module
//	  (import "wasi_snapshot_preview1" "fd_write"
//	    (func $fd_write (param i32 i32 i32 i32) (result i32)))
//	  (memory (export "memory") 1)
//	  (data (i32.const 0) "\10\00\00\00\10\00\00\00") ; iovec: ptr 16, len 16
//	  (data (i32.const 16) "hello from wasm\n")
//	  (func (export "_start")
//	    i32.const 1   ;; stdout
//	    i32.const 0   ;; iovec
//	    i32.const 1   ;; iovec count
//	    i32.const 8   ;; where to write the byte count
//	    call $fd_write
//	    drop))
//
// The bytes are spelled out instead of using a toolchain so the example stays
// dependency-free. A real project would compile a .wasm file with TinyGo or
// wasi-sdk and pass those bytes instead.
func helloWASM() []byte {
	const message = "hello from wasm\n"
	iovec := []byte{
		16, 0, 0, 0, // buffer offset in linear memory
		16, 0, 0, 0, // buffer length
	}

	types := wasmSection(1,
		uleb(2), // two function types
		[]byte{0x60, 0x04, 0x7f, 0x7f, 0x7f, 0x7f, 0x01, 0x7f}, // (i32 x4) -> i32: fd_write
		[]byte{0x60, 0x00, 0x00},                               // () -> (): _start
	)
	imports := wasmSection(2,
		uleb(1),
		wasmString("wasi_snapshot_preview1"),
		wasmString("fd_write"),
		[]byte{0x00, 0x00}, // function kind, type 0
	)
	functions := wasmSection(3, uleb(1), uleb(1))           // _start has type 1
	memories := wasmSection(5, uleb(1), []byte{0x00, 0x01}) // one memory, minimum 1 page
	exports := wasmSection(7,
		uleb(2),
		wasmString("memory"), []byte{0x02, 0x00},
		wasmString("_start"), []byte{0x00, 0x01}, // function index 1 (fd_write is 0)
	)
	code := wasmSection(10,
		uleb(1),
		[]byte{
			13,         // body size
			0x00,       // no locals
			0x41, 0x01, // i32.const 1 (stdout)
			0x41, 0x00, // i32.const 0 (iovec)
			0x41, 0x01, // i32.const 1 (iovec count)
			0x41, 0x08, // i32.const 8 (nwritten)
			0x10, 0x00, // call fd_write
			0x1a, // drop
			0x0b, // end
		},
	)
	data := wasmSection(11,
		uleb(2),
		[]byte{0x00, 0x41, 0x00, 0x0b}, // active, offset 0
		uleb(len(iovec)), iovec,
		[]byte{0x00, 0x41, 0x10, 0x0b}, // active, offset 16
		uleb(len(message)), []byte(message),
	)

	return join(
		[]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, // \0asm, version 1
		types, imports, functions, memories, exports, code, data,
	)
}

// uleb encodes an unsigned LEB128 integer.
func uleb(n int) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}

// wasmString encodes a length-prefixed WebAssembly name.
func wasmString(s string) []byte {
	return append(uleb(len(s)), s...)
}

// wasmSection encodes a section with its id and byte length.
func wasmSection(id byte, parts ...[]byte) []byte {
	payload := join(parts...)
	out := []byte{id}
	out = append(out, uleb(len(payload))...)
	return append(out, payload...)
}

func join(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
