#!/usr/bin/env bash
# Fetches the pinned, checksum-verified toolchain used to build runtime.wasm.
#
# Usage: source scripts/toolchain.sh   (exports WASI_SDK, BINARYEN, WIZER)
#
# Every version and archive SHA-256 is pinned here. Bumping a tool means
# changing its version + checksums in this file and committing the rebuilt
# runtime.wasm together with its new runtime.wasm.sha256 (see README,
# "Update policy"). The Rust toolchain is pinned in
# kafig-runtime/rust-toolchain.toml and installed by rustup.
#
# The canonical (checksum-of-record) build host is linux/x86_64, which is what
# CI uses. darwin/arm64 archives are provided for local development; artifacts
# built there are expected but not guaranteed to be byte-identical.
set -euo pipefail

KAFIG_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KAFIG_TOOLCHAIN_DIR="${KAFIG_TOOLCHAIN_DIR:-$KAFIG_ROOT/.toolchain}"

WASI_SDK_VERSION=34
WASI_SDK_FULL_VERSION=34.0
BINARYEN_VERSION=133
WIZER_VERSION=11.0.3

case "$(uname -s)-$(uname -m)" in
  Linux-x86_64)
    WASI_SDK_PLATFORM=x86_64-linux
    WASI_SDK_SHA256=b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4
    BINARYEN_PLATFORM=x86_64-linux
    BINARYEN_SHA256=2dc9c7813f5375db93d96ead4b78222fcc3e2677bbb832297af4797782a37489
    WIZER_PLATFORM=x86_64-linux
    WIZER_SHA256=36016e7f846f38ad09f22eb4bafb99068f038c431eb9613ddf59e31ad8a21414
    ;;
  Darwin-arm64)
    WASI_SDK_PLATFORM=arm64-macos
    WASI_SDK_SHA256=9c59398106b417f8f14913380fdf0097a8cc0ff4af9eb3ce0065a859e88d49e9
    BINARYEN_PLATFORM=arm64-macos
    BINARYEN_SHA256=ad66da82ac13f163e424b1643f16c6dfcccc98b5966296b43e52d3cab04f84a8
    WIZER_PLATFORM=aarch64-macos
    WIZER_SHA256=7c54896b456b6ca024af6005ebc5538282dee7cd9f0c9c1155855d29b9c461b1
    ;;
  *)
    echo "toolchain.sh: unsupported build host $(uname -s)-$(uname -m) (supported: Linux-x86_64, Darwin-arm64)" >&2
    return 1 2>/dev/null || exit 1
    ;;
esac

_kafig_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# _kafig_fetch <url> <sha256> <dest-dir>: download, verify, unpack (strip 1).
_kafig_fetch() {
  local url="$1" sha="$2" dest="$3"
  if [[ -f "$dest/.complete" ]] && [[ "$(cat "$dest/.complete")" == "$sha" ]]; then
    return 0
  fi
  rm -rf "$dest" && mkdir -p "$dest"
  local archive="$dest.download"
  echo "toolchain.sh: fetching $url" >&2
  curl -fsSL --retry 3 -o "$archive" "$url"
  local got
  got="$(_kafig_sha256 "$archive")"
  if [[ "$got" != "$sha" ]]; then
    echo "toolchain.sh: checksum mismatch for $url: want $sha, got $got" >&2
    rm -f "$archive"
    return 1
  fi
  tar -xf "$archive" -C "$dest" --strip-components=1
  rm -f "$archive"
  echo "$sha" > "$dest/.complete"
}

mkdir -p "$KAFIG_TOOLCHAIN_DIR"

WASI_SDK="$KAFIG_TOOLCHAIN_DIR/wasi-sdk-$WASI_SDK_FULL_VERSION"
_kafig_fetch \
  "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-$WASI_SDK_VERSION/wasi-sdk-$WASI_SDK_FULL_VERSION-$WASI_SDK_PLATFORM.tar.gz" \
  "$WASI_SDK_SHA256" "$WASI_SDK"

BINARYEN="$KAFIG_TOOLCHAIN_DIR/binaryen-$BINARYEN_VERSION"
_kafig_fetch \
  "https://github.com/WebAssembly/binaryen/releases/download/version_$BINARYEN_VERSION/binaryen-version_$BINARYEN_VERSION-$BINARYEN_PLATFORM.tar.gz" \
  "$BINARYEN_SHA256" "$BINARYEN"

WIZER_DIR="$KAFIG_TOOLCHAIN_DIR/wizer-$WIZER_VERSION"
_kafig_fetch \
  "https://github.com/bytecodealliance/wizer/releases/download/v$WIZER_VERSION/wizer-v$WIZER_VERSION-$WIZER_PLATFORM.tar.xz" \
  "$WIZER_SHA256" "$WIZER_DIR"
WIZER="$WIZER_DIR/wizer"

export KAFIG_ROOT KAFIG_TOOLCHAIN_DIR WASI_SDK BINARYEN WIZER
