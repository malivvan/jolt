# POST.md — follow-ups after WASI auto-loading

`PLAN.md` is fully implemented (Phases A–D) and all verification commands pass:

```sh
go build ./...
go build -tags canopy_wasm ./...
go vet -tags canopy_wasm ./...
go test ./...
go test -tags canopy_wasm ./...
```

This file lists what was intentionally left out, what should be checked next,
and repository problems that predate this work.

## 1. Repository hygiene (pre-existing, partially fixed)

The initial commit did not compile. A prerequisite commit (`4e39ac5`) repaired
just enough to build and test:

- Tests/examples imported `"canopy"` while the module is
  `github.com/malivvan/js`. They now import the real path, aliased as `canopy`
  in `more_test.go`, `example_test.go` and `examples/*/main.go`, so the
  identifiers still read `canopy.` and the diff stays small.
- `js_test.go` was `package js` while importing its own package (an import
  cycle). It is now `package js_test`; the `var js *js.JSError` shadow was
  renamed to `jsErr`.
- Every file carried the same `wazero && !(js && wasm)` build tag, so the
  package could not build untagged and `wasm.go` collided with `wasm_off.go`.
  Core files are untagged, WebAssembly files use
  `canopy_wasm && !(js && wasm)`, and `wasm_off.go` has the complement.

Still to do, as a deliberate separate change:

- [ ] Finish the rename: the package is still `js` while docs, error prefixes
      (`"canopy: ..."`), exported doc comments and the example aliases say
      `canopy`. Decide whether the public package name stays `js` or the module
      path changes; then update the aliases to the final name.
- [ ] `doc.go`'s package comment still describes a package named `canopy`.
- [ ] Add CI that runs both tag modes. An untagged-only CI run silently skips
      the entire WebAssembly backend.
- [ ] Run `go test -race ./...` and `-race` with `-tags canopy_wasm`; the WASI
      host module is shared per runtime, which is the first place a race would
      show up.

## 2. WASI scope deliberately not covered

Design decision 4 limited the work to `wasi_snapshot_preview1`. Possible next
steps, roughly in value order:

- [ ] `wasi_unstable` alias, using
      `wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(builder)`
      and a host module named `wasi_unstable`. Remember host module names are
      unique per `wazero.Runtime`.
- [ ] `env` / `assemblyscript` / `emscripten` host namespaces. These need
      policy decisions (which imports are satisfied by the host, and how
      JavaScript can override them) rather than just wiring.
- [ ] Clocks: expose `WithSysWalltime`, `WithSysNanotime`, `WithNanosleep` and
      `WithOsyield` options. They are deliberately absent today, so
      `clock_time_get(REALTIME)` returns `ENOSYS`. Add explicit opt-ins so the
      sandbox default stays intact.
- [ ] Rich `FS` configuration: the current option accepts a plain `fs.FS`
      (`WithFS`). Add `WithFSConfig`-style mounts, read-only mounts and write
      capability, plus an `Allow` hook analogous to `HostFS`.
- [ ] Reactor modules: `_initialize` is not run. If reactors matter, add a
      separate opt-in (e.g. `RunInitialize`) instead of overloading
      `RunStart`.
- [ ] Per-instance environment: `WebAssembly.WASI` is state-wide, so every
      instance in an `Executor` shares args/env/FS/stdio. If callers need
      per-call environments, add a way to override the WASI block at
      instantiation time.

## 3. Semantics worth confirming / documenting

- [ ] **Import precedence is not overridable.** A user-supplied
      `wasi_snapshot_preview1` in `importObject` is ignored while WASI is
      enabled (design decision 2). This is documented, but there is no test for
      it, and no way to opt back into a user-supplied WASI object short of
      `WASI.Disable`. Add a precedence test and decide whether a more granular
      override is wanted.
- [ ] **`RunStart` is silent when `_start` is absent.** It only runs the export
      if present. Consider logging or returning an error for command modules
      that expect it.
- [ ] **`RunStart` is not tied to WASI usage.** It runs whenever WASI is
      enabled and the flag is set, even for non-WASI modules. That is
      convenient but should be stated in the option docs.
- [ ] **Exports after `proc_exit`.** `proc_exit` closes the guest, so
      `buildExports` skips closed handles and the instance may expose fewer
      exports than declared. Document this on `RunStart`.
- [ ] **Non-zero `proc_exit` classification.** It currently becomes a
      `WebAssembly.RuntimeError` whose message comes from `sys.ExitError`. If
      callers need the exit code, expose a dedicated error type or wrap it.
- [ ] **`WASI.Disable` with a user-supplied namespace** should keep working
      through the synthetic-namespace path, but it is untested.

## 4. Tests to add

Current coverage is in `wasm_wasi_test.go`: auto-instantiation, mixed
WASI/JS imports, args plumbing, `RunStart`, `proc_exit(0)`, non-zero
`proc_exit`, and `Disable`.

- [ ] `env` and `environ_get` plumbing (args is covered; env uses the same
      `ModuleConfig` path but should be asserted).
- [ ] `FS` plumbing with an `fstest.MapFS` and a small `path_open`/`fd_read`
      module.
- [ ] `Stdout`/`Stderr` plumbing via `fd_write`.
- [ ] `Rand` plumbing via `random_get` (nil must stay deterministic).
- [ ] Precedence: a throwing `importObject.wasi_snapshot_preview1.args_get`
      must not be called while WASI is enabled.
- [ ] One end-to-end module produced by TinyGo `-target=wasi` or wasi-sdk
      (kept out of the repo, generated under `$TMPDIR`) to validate against
      real toolchain output rather than hand-assembled bytes.
- [ ] Context cancellation during a long-running `_start` (the runtime uses
      `WithCloseOnContextDone(true)`).

## 5. Performance / lifecycle

- [ ] WASI is instantiated eagerly for every pooled runtime, even when the
      program never uses WebAssembly. Measure the per-runtime cost; if it
      matters, switch to the documented lazy `sync.Once` load on first use.
- [ ] `wasmState` only counts instances and host modules; confirm the
      `wasi_snapshot_preview1` host module and compiled guest modules are
      closed by the existing `wr.Close` cleanup (they should be, since the
      runtime owns them) and add a leak test if this area changes.
- [ ] Guest modules are compiled per `WebAssembly.instantiate` call, as
      before. WASI does not change this, but repeated instantiation of the
      same bytes is still a likely performance target.

## 6. Documentation

- [ ] `README.md` and `doc.go` now describe WASI, `WASI.Disable`, `RunStart`
      and the sandboxed defaults. Consider a runnable `example_test.go`
      example for a WASI command module.
- [ ] `config.go`'s `WebAssembly` field comment points at
      `WebAssembly.WASI`; a field-by-field table in the README may be easier
      to scan than the nested option list.
- [ ] `WAZERO.md` is the research record and was left untouched. Its §9 file
      map and "recommended plan" now describe implemented work; keep it as
      history or add a pointer to `PLAN.md`.

## 7. Security review

- [ ] The WASI `FS` option accepts any `io/fs.FS` without the `Allow` hook that
      `HostFS` offers. Treat a non-nil `FS`, real `Stdout`/`Stderr` and
      `Rand` as capabilities and document that clearly (the README security
      section was updated, but no API-level guard exists).
- [ ] `random_get` uses wazero's deterministic source when `Rand` is nil. That
      is safe, but code relying on randomness will be predictable; the docs
      say so, and a warning or explicit option could make it harder to miss.
- [ ] `WASI.Args`/`Env` are copied into each guest's `sys.Context`, not shared
      mutably; confirm with a test if multiple instances run concurrently.
