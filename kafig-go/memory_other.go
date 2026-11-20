//go:build !((linux || darwin) && (amd64 || arm64))

package kafig

import "github.com/tetratelabs/wazero/experimental"

const mmapMemorySupported = false

type mmapAllocator struct{ mem *mmapMemory }

func (*mmapAllocator) Allocate(_, _ uint64) experimental.LinearMemory { return nil }

type mmapMemory struct{}

func (*mmapMemory) release() {}
