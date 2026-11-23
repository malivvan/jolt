//go:build nowasm || (js && wasm)

package jolt

import "errors"

// WebAssemblySupported reports whether this binary includes the WebAssembly
// JavaScript API. It is false when jolt is built with the nowasm tag or for
// GOARCH=wasm.
const WebAssemblySupported = false

// installWebAssembly is the disabled implementation. The WebAssembly global is
// simply absent; explicit memory or WASI configuration is an error so a build
// without the backend does not silently ignore it.
func installWebAssembly(env *Env, opts WasmOptions) (func() error, error) {
	if opts.Disable {
		return nil, nil
	}
	if opts.MemoryLimitPages != 0 || opts.WASI.configured() {
		return nil, errors.New("jolt: WebAssembly support is disabled in this build (nowasm tag or js/wasm)")
	}
	return nil, nil
}
