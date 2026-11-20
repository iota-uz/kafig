# Kafig Runtime

Rust crate that embeds QuickJS-ng (via rquickjs) and is compiled to
`wasm32-wasip1`, pre-initialized with Wizer and embedded into kafig-go as
`kafig-go/internal/wasm/runtime.wasm`.

## Build

Everything is pinned and fetched by the scripts; the only prerequisites are
`rustup`, `curl`, `tar`, and Go (for `tools/wasm-merge-data`).

```shell
../scripts/build-wasm.sh            # build into target/runtime.wasm
../scripts/build-wasm.sh --install  # also copy into kafig-go + update .sha256
../scripts/build-wasm.sh --check    # verify against the committed runtime.wasm
```

`make build|install|check` are thin wrappers. Supported build hosts:
linux/x86_64 (canonical, used by CI) and darwin/arm64.

## Layout

- `src/` — the runtime (exports `eval`, `eval_compiled`, `dispatch_event`,
  `resolve_rpc`, ...; see `../PROTOCOL.md`)
- `vendor/rquickjs-sys/` — QuickJS-ng sources, re-created by
  `../scripts/vendor-rquickjs-sys.sh` from crates.io plus `patches/`
- `wizer-*-stub.wat` — deterministic imports used only while Wizer snapshots
  the module
