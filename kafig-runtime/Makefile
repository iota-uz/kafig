# Thin wrapper around the reproducible build (see ../scripts/build-wasm.sh).
.PHONY: build install check clean

build:
	../scripts/build-wasm.sh

install:
	../scripts/build-wasm.sh --install

check:
	../scripts/build-wasm.sh --check

clean:
	cargo clean
