//go:build !canopy_wasm || (js && wasm)

package js

import "errors"

// WebAssemblySupported reports whether this binary was built with the wasm
// build tag and therefore can install the WebAssembly JavaScript API.
const WebAssemblySupported = false

// installWebAssembly is the no-tag implementation. The WebAssembly global is
// simply absent; explicit memory configuration is an error so a mis-built
// binary does not silently ignore it.
func installWebAssembly(env *Env, opts WebAssemblyOptions) (func() error, error) {
	if opts.Disable {
		return nil, nil
	}
	if opts.MemoryLimitPages != 0 {
		return nil, errors.New("canopy: WebAssembly support requires building with -tags canopy_wasm")
	}
	return nil, nil
}
