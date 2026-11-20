#!/usr/bin/env bash
# Reproducible build of kafig-go/internal/wasm/runtime.wasm.
#
#   scripts/build-wasm.sh            build into kafig-runtime/target/runtime.wasm
#   scripts/build-wasm.sh --install  build and copy into kafig-go/internal/wasm
#   scripts/build-wasm.sh --check    build and fail unless the result matches
#                                    the committed runtime.wasm and its .sha256
#
# Reproducibility measures:
#   * every tool is pinned (rust-toolchain.toml, scripts/toolchain.sh);
#   * Cargo.lock is used with --locked;
#   * host paths are remapped (Rust: --remap-path-prefix, C: -ffile-prefix-map)
#     so the checkout, CARGO_HOME and toolchain locations do not leak in;
#   * the post-processing tool (tools/wasm-merge-data) is deterministic;
#   * the Wizer snapshot runs WITHOUT host WASI: WASI is replaced by the fixed
#     stub in kafig-runtime/wizer-wasi-stub.wat (constant clock and zeroed
#     randomness), so the snapshot cannot capture build-time nondeterminism.
set -euo pipefail

MODE="build"
case "${1:-}" in
  "") ;;
  --install) MODE="install" ;;
  --check) MODE="check" ;;
  *) echo "usage: $0 [--install|--check]" >&2; exit 2 ;;
esac

# shellcheck source=toolchain.sh
source "$(dirname "${BASH_SOURCE[0]}")/toolchain.sh"

RUNTIME_DIR="$KAFIG_ROOT/kafig-runtime"
TARGET_DIR="$RUNTIME_DIR/target"
OUT="$TARGET_DIR/runtime.wasm"
GO_WASM="$KAFIG_ROOT/kafig-go/internal/wasm/runtime.wasm"
GO_WASM_SUM="$GO_WASM.sha256"
CARGO_HOME="${CARGO_HOME:-$HOME/.cargo}"

export SOURCE_DATE_EPOCH=0
export CARGO_INCREMENTAL=0
export CARGO_TARGET_WASM32_WASIP1_LINKER="$WASI_SDK/bin/wasm-ld"
export CC_wasm32_wasip1="$WASI_SDK/bin/clang"
export CXX_wasm32_wasip1="$WASI_SDK/bin/clang++"
export AR_wasm32_wasip1="$WASI_SDK/bin/llvm-ar"

remap_rust="--remap-path-prefix=$RUNTIME_DIR=/kafig-runtime --remap-path-prefix=$CARGO_HOME=/cargo --remap-path-prefix=$KAFIG_TOOLCHAIN_DIR=/toolchain"
remap_c="--sysroot=$WASI_SDK/share/wasi-sysroot -ffile-prefix-map=$RUNTIME_DIR=/kafig-runtime -ffile-prefix-map=$CARGO_HOME=/cargo -ffile-prefix-map=$KAFIG_TOOLCHAIN_DIR=/toolchain"
export CARGO_TARGET_WASM32_WASIP1_RUSTFLAGS="$remap_rust"
# Note: cc gives CFLAGS_<target> precedence over the CFLAGS that
# rquickjs-sys build.rs sets, hence the explicit --sysroot above.
export CFLAGS_wasm32_wasip1="$remap_c"

cd "$RUNTIME_DIR"
cargo build --locked --release --target wasm32-wasip1

RAW="$TARGET_DIR/wasm32-wasip1/release/kafig-runtime.wasm"
"$BINARYEN/bin/wasm-opt" -O3 "$RAW" -o "$TARGET_DIR/runtime-opt.wasm"
"$WIZER" "$TARGET_DIR/runtime-opt.wasm" \
  --init-func wizer.initialize \
  --preload "wasi_snapshot_preview1=$RUNTIME_DIR/wizer-wasi-stub.wat" \
  --preload "env=$RUNTIME_DIR/wizer-env-stub.wat" \
  --inherit-stdio false \
  -o "$TARGET_DIR/runtime-initialized.wasm"
"$BINARYEN/bin/wasm-opt" -O3 --strip-debug --strip-producers \
  "$TARGET_DIR/runtime-initialized.wasm" -o "$TARGET_DIR/runtime-final.wasm"
# Wizer emits thousands of tiny data segments; coalesce them (must run last:
# wasm-opt's memory-packing would split them again). Cuts instantiation time.
(cd "$KAFIG_ROOT/tools" && GOWORK=off go run ./wasm-merge-data \
  "$TARGET_DIR/runtime-final.wasm" "$OUT")

sum="$(_kafig_sha256 "$OUT")"
echo "runtime.wasm sha256: $sum"

case "$MODE" in
  install)
    cp "$OUT" "$GO_WASM"
    echo "$sum  runtime.wasm" > "$GO_WASM_SUM"
    echo "installed $GO_WASM"
    ;;
  check)
    committed="$(_kafig_sha256 "$GO_WASM")"
    recorded="$(cut -d' ' -f1 "$GO_WASM_SUM")"
    if [[ "$sum" != "$committed" || "$sum" != "$recorded" ]]; then
      echo "MISMATCH: built $sum, committed runtime.wasm $committed, runtime.wasm.sha256 $recorded" >&2
      exit 1
    fi
    echo "OK: built runtime.wasm matches the committed file and checksum"
    ;;
esac
