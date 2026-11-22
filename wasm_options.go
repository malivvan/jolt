//go:build wazero && !(js && wasm)

package js

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
}
