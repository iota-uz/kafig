package kafig

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// QuickJS bytecode is not validated when loaded: a crafted bytecode buffer
// can corrupt the interpreter's memory and escape JS-level isolation (it stays
// inside the WASM sandbox, but can reach the prelude internals, other
// handlers' state, etc.). kafig therefore accepts only source code from
// callers. Runtime.Compile seals the bytecode it produces with an HMAC under a
// per-Runtime random key, and EvalCompiled refuses anything it did not seal.

const bytecodeTagSize = sha256.Size

// ErrForeignBytecode is returned by EvalCompiled for bytecode that was not
// produced by Compile on the same Runtime (or was modified afterwards).
var ErrForeignBytecode = errors.New("kafig: bytecode was not produced by this Runtime's Compile")

func newBytecodeKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("kafig: bytecode key: %w", err)
	}
	return key, nil
}

func bytecodeTag(key, bytecode []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(bytecode)
	return m.Sum(nil)
}

// sealBytecode returns bytecode followed by its HMAC tag.
func sealBytecode(key, bytecode []byte) []byte {
	out := make([]byte, 0, len(bytecode)+bytecodeTagSize)
	out = append(out, bytecode...)
	return append(out, bytecodeTag(key, bytecode)...)
}

// openBytecode verifies the tag appended by sealBytecode and returns the raw
// QuickJS bytecode.
func openBytecode(key, sealed []byte) ([]byte, error) {
	if len(sealed) <= bytecodeTagSize {
		return nil, ErrForeignBytecode
	}
	bytecode := sealed[:len(sealed)-bytecodeTagSize]
	if !hmac.Equal(sealed[len(bytecode):], bytecodeTag(key, bytecode)) {
		return nil, ErrForeignBytecode
	}
	return bytecode, nil
}
