package js

import (
	"io"
	"io/fs"
)

// WebAssemblyOptions configures the WebAssembly JavaScript API.
//
// The API is only compiled into the binary when canopy is built with the
// "canopy_wasm" build tag:
//
//	go build -tags canopy_wasm ./...
//
// The obvious tag name "wasm" cannot be used: Go reserves it for
// GOARCH=wasm, and passing -tags wasm on another architecture makes the
// standard library select its wasm files and fail to compile.
//
// Without the tag the WebAssembly global is not installed.
// WebAssemblySupported reports which build is in use.
type WebAssemblyOptions struct {
	// Disable leaves the WebAssembly global uninstalled even when canopy is
	// built with the wasm tag. It is ignored without the tag.
	Disable bool
	// MemoryLimitPages caps each WebAssembly linear memory in 64 KiB pages.
	// Zero means 65536, the browser limit (4 GiB).
	MemoryLimitPages uint32
	// WASI configures the wasi_snapshot_preview1 host module and the
	// environment attached to each guest that imports it.
	WASI WebAssemblyWASIOptions
}

// WebAssemblyWASIOptions configures WASI support. When canopy is built with
// the canopy_wasm tag, the wasi_snapshot_preview1 host module is always
// instantiated so WASI-targeting modules load without the caller supplying
// WASI imports; set Disable to turn that off.
//
// The environment fields below are attached to every instantiated module that
// imports wasi_snapshot_preview1. Defaults are deliberately sandboxed: no
// arguments, no environment, no host file system, no stdio, and wazero's
// deterministic random_get source.
type WebAssemblyWASIOptions struct {
	// Disable leaves the WASI host module unloaded. A module that imports
	// wasi_snapshot_preview1 then needs the caller to supply those imports.
	Disable bool
	// Args is the argument vector reported by args_get.
	Args []string
	// Env is the environment reported by environ_get.
	Env map[string]string
	// FS is the guest file system; nil means no host file system.
	FS fs.FS
	// Stdin, Stdout and Stderr are the guest's standard streams.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Rand seeds random_get; nil uses wazero's deterministic default.
	Rand io.Reader
	// RunStart runs the "_start" export right after instantiation. The
	// browser default is to leave it to the caller.
	RunStart bool
}

// configured reports whether the caller asked for any WASI behavior. Disable
// on its own is not counted: without the build tag WASI is absent anyway.
func (o WebAssemblyWASIOptions) configured() bool {
	return len(o.Args) > 0 || len(o.Env) > 0 || o.FS != nil ||
		o.Stdin != nil || o.Stdout != nil || o.Stderr != nil ||
		o.Rand != nil || o.RunStart
}
