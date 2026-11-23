# AGENTS.md

`jolt` — a developer-focused JavaScript executor for Go, built on
[moejs](https://github.com/MoeForge/moejs), with a browser-style WebAssembly
API backed by wazero.

## Project

- Go module: `github.com/malivvan/jolt`. The package is named `jolt`.
- Entry points: `jolt.New(Config)` returns an `*Executor` (compiled once,
  pooled runtimes); `jolt.NewRuntime(Config)` returns one owned `*Runtime`.
- Manifest: `go.mod` (Go 1.25). README is the user-facing guide; `doc.go`
  holds the package docs; `LICENSE` is MIT.
- WebAssembly is part of the default build. `-tags nowasm` removes it.

## Layout

- Repository root: the `jolt` package and its tests.
- `demos/`: runnable, dependency-free programs the CI smoke test runs.
- `vendor/`: wazero and moejs, vendored for reproducible builds.

## Commands

```sh
go build ./...                  # default build, WebAssembly included
go build -tags nowasm ./...     # WebAssembly backend excluded
go vet ./... && go vet -tags nowasm ./...
go test ./... && go test -tags nowasm ./...
go test -race ./...             # nightly/CI signal
gofmt -l .                      # repository is gofmt-clean

go test -coverprofile=coverage.out .    # root package; gate is >= 90%
go tool cover -func=coverage.out | tail -1

go run ./demos/greet         # hello world!
go run ./demos/sandbox       # 42
go run ./demos/webassembly   # hello from wasm / ok
```

CI lives in `.github/workflows/ci.yml`: both build modes, race detector,
the demos smoke test, a 90% coverage gate, and staticcheck.

## Architecture

| File(s) | Role |
| --- | --- |
| `config.go` | `Config`, defaults/validation, `WebAssemblyOptions` and WASI options |
| `executor.go` | `Executor`, program compilation, bounded runtime pool (`New`, `Call`, `Eval`, `Get`) |
| `runtime.go` | `Runtime`: one moejs runtime, event loop, calling/eval, exports |
| `features.go` | `Feature`/`FeatureFunc`, `Env`, install and cleanup ordering |
| `funcs.go`, `value.go` | Go↔JS value conversion, host functions, JS handles |
| `fs.go`, `modules.go`, `console.go` | `HostFS`/`NoFS`, module resolution/loaders, console |
| `errors.go` | `*Error`, `JSError`, `IsTimeout`, `IsException`, `JSStack` helpers |
| `wasm.go` | WebAssembly JS API on wazero, WASI host module, module binary parser |
| `wasm_values.go` | Memory/global/table bridging, value encoding, export building |
| `wasm_off.go` | Disabled-backend stub (`-tags nowasm` or `GOARCH=wasm`) |

### Build tags

- WebAssembly implementation files use
  `//go:build !nowasm && !(js && wasm)` — **the backend is on by default**.
  The `js` and `wasm` tags are the GOOS and GOARCH of the
  `GOOS=js GOARCH=wasm` target, not the old package name: the guard excludes
  the wazero backend when cross-compiling for the browser/Node target.
- `wasm_off.go` uses the complement, `//go:build nowasm || (js && wasm)`.
- `WebAssemblyOptions` and `WebAssemblyWASIOptions` live in `config.go`
  (untagged) because `Config` and the disabled stub reference them in both
  modes. Do not move them behind the wasm tag.
- Never introduce a `wasm` build tag for the feature: `wasm` is reserved by
  Go for `GOARCH=wasm`, and `-tags wasm` makes the standard library select
  wasm sources and fail. `nowasm` is the opt-out.
- Keep the `!(js && wasm)` guard on the implementation: wazero is not built
  for `GOOS=js GOARCH=wasm`.

## Conventions

- `gofmt` is the only formatter; all files are gofmt-clean.
- Exported API needs concise doc comments. Errors created by jolt use the
  `"jolt: ..."` prefix; wrap engine errors so `errors.Is`/`errors.As` work.
- The public module path is `github.com/malivvan/jolt`. Do not reintroduce the
  old `js` name in import paths, error prefixes or documentation (the `js`
  build tag is the GOOS tag and stays).
- Features are composable: new globals/filesystems/network integrations
  implement `Feature`, not ad-hoc setup in `New`.
- WASI defaults are sandboxed (no args/env/FS/stdio, deterministic random).
  Host access is opt-in through `WebAssembly.WASIOptions`; do not change the
  defaults silently.
- WebAssembly tests are hand-assembled binaries. Reuse the helpers in
  `wasm_test.go` (`testModule`, `testTypeSection`, `testImportSection`,
  `testFunctionSection`, `testMemorySection`, `testExportSection`,
  `testCodeSection`) instead of adding `.wasm` fixtures.
- Tests use the standard `testing` package; keep broad internal coverage in
  `coverage_test.go` and WebAssembly coverage in `wasm_test.go` rather than
  spreading many small test files.
- Do not commit generated `.wasm` artifacts outside `demos/`; the demo
  assembles its module in Go.

## Notes

<!-- Add short-lived, project-specific notes here. -->
