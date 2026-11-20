#!/usr/bin/env bash
# Re-creates kafig-runtime/vendor/rquickjs-sys from the crates.io release
# pinned below plus the patches in kafig-runtime/patches/rquickjs-sys/.
#
# The crate is vendored only because QuickJS-ng needs source patches for the
# sandbox (see the patch files for the why). To update QuickJS-ng / rquickjs:
#   1. bump RQUICKJS_SYS_VERSION + RQUICKJS_SYS_SHA256 (the "checksum" that
#      `cargo update -p rquickjs-sys` writes for the release into a scratch
#      Cargo.lock, or the sha256 of the downloaded .crate file);
#   2. run this script; refresh any patch that no longer applies;
#   3. bump `rquickjs` in kafig-runtime/Cargo.toml to the matching release;
#   4. rebuild and commit runtime.wasm (scripts/build-wasm.sh --install).
set -euo pipefail

RQUICKJS_SYS_VERSION=0.11.0
RQUICKJS_SYS_SHA256=27344601ef27460e82d6a4e1ecb9e7e99f518122095f3c51296da8e9be2b9d83

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="$ROOT/kafig-runtime/vendor/rquickjs-sys"
PATCHES="$ROOT/kafig-runtime/patches/rquickjs-sys"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

crate="$work/rquickjs-sys.crate"
curl -fsSL --retry 3 -o "$crate" \
  "https://static.crates.io/crates/rquickjs-sys/rquickjs-sys-$RQUICKJS_SYS_VERSION.crate"
if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$crate" | cut -d' ' -f1)"
else got="$(shasum -a 256 "$crate" | cut -d' ' -f1)"; fi
if [[ "$got" != "$RQUICKJS_SYS_SHA256" ]]; then
  echo "checksum mismatch: want $RQUICKJS_SYS_SHA256, got $got" >&2
  exit 1
fi
tar -xzf "$crate" -C "$work"
src="$work/rquickjs-sys-$RQUICKJS_SYS_VERSION"

# Keep only what build.rs needs for the wasm32-wasip1 target.
rm -rf "$DEST"
mkdir -p "$DEST/src/bindings" "$DEST/src/inlines" "$DEST/quickjs"
cp "$src/Cargo.toml" "$src/build.rs" "$src/quickjs.bind.h" "$src/README.md" "$DEST/"
cp "$src/src/lib.rs" "$DEST/src/"
cp "$src"/src/inlines/*.rs "$DEST/src/inlines/"
cp "$src/src/bindings/wasm32-wasip1.rs" "$DEST/src/bindings/"
for f in LICENSE builtin-array-fromasync.h dtoa.h libregexp-opcode.h libregexp.h \
  libunicode-table.h libunicode.h list.h quickjs-atom.h quickjs-opcode.h \
  quickjs-c-atomics.h quickjs.h cutils.h libregexp.c libunicode.c cutils.c \
  quickjs.c dtoa.c; do
  cp "$src/quickjs/$f" "$DEST/quickjs/"
done

for p in "$PATCHES"/*.patch; do
  [[ -e "$p" ]] || continue
  echo "applying $(basename "$p")"
  patch -d "$DEST" -p1 --no-backup-if-mismatch < "$p"
done
echo "vendored rquickjs-sys $RQUICKJS_SYS_VERSION into $DEST"
