//go:build (linux || darwin) && (amd64 || arm64)

package kafig

import (
	"sync"
	"syscall"

	"github.com/tetratelabs/wazero/experimental"
)

// mmapMemorySupported reports whether this platform has the mmap-backed
// linear memory allocator.
const mmapMemorySupported = true

// System calls used by the allocator; replaced in tests to simulate failure.
var (
	sysMmap     = syscall.Mmap
	sysMprotect = syscall.Mprotect
	sysMunmap   = syscall.Munmap
)

// mmapAllocator backs one Instance's WASM linear memory with an anonymous
// mapping instead of a Go-heap []byte. A new allocator is used per Instance
// so the Instance can find (and later unmap) its memory.
//
// The whole maximum size is reserved up front with PROT_NONE (address space
// only, nothing committed) and the accessible prefix is extended with
// mprotect as the guest grows its memory. Consequences:
//   - instantiation does not zero the full initial memory: the kernel hands
//     out zero pages lazily, so only touched pages cost time and RSS;
//   - growing never copies (the base address is stable);
//   - Instance.Close returns the memory to the OS immediately instead of
//     waiting for the Go GC.
//
// wazero (1.11) does not handle a nil LinearMemory from Allocate nor a nil
// buffer from the first Reallocate: both panic. Allocate therefore commits the
// initial capacity itself and, if mmap or mprotect fails (address-space or
// map-count limits), falls back to a Go-heap buffer bounded by the same
// maximum. Only later growth can fail, which the guest sees as a failed
// memory.grow (out of memory), not a panic.
type mmapAllocator struct {
	mem *mmapMemory // nil when the heap fallback was used
}

func (a *mmapAllocator) Allocate(capacity, maxBytes uint64) experimental.LinearMemory {
	if maxBytes < capacity {
		maxBytes = capacity
	}
	if m := newMmapMemory(capacity, maxBytes); m != nil {
		a.mem = m
		return m
	}
	return newHeapMemory(capacity, maxBytes)
}

func newMmapMemory(capacity, maxBytes uint64) *mmapMemory {
	reserve := max(pageAlignUp(maxBytes), uint64(syscall.Getpagesize()))
	if reserve > uint64(^uint(0)>>1) {
		return nil
	}
	buf, err := sysMmap(-1, 0, int(reserve), syscall.PROT_NONE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil || uint64(len(buf)) < reserve {
		return nil
	}
	m := &mmapMemory{reserved: buf}
	if need := pageAlignUp(capacity); need > 0 {
		if err := sysMprotect(buf[:need], syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
			_ = sysMunmap(buf)
			return nil
		}
		m.committed = need
	}
	return m
}

type mmapMemory struct {
	reserved  []byte // the whole reservation
	committed uint64 // bytes currently PROT_READ|PROT_WRITE
	once      sync.Once
}

func (m *mmapMemory) Reallocate(size uint64) []byte {
	if size > uint64(len(m.reserved)) {
		return nil
	}
	if need := pageAlignUp(size); need > m.committed {
		if err := sysMprotect(m.reserved[m.committed:need], syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
			return nil
		}
		m.committed = need
	}
	return m.reserved[:size:size]
}

// Free is called by wazero when the module closes. That can happen on
// wazero's context watcher goroutine (WithCloseOnContextDone) while guest
// code is still running on the Eval goroutine, which only notices the close
// at its next termination check and keeps touching memory until then.
// Unmapping here would turn that into a SIGSEGV, so the mapping is released
// by release() instead: from Instance.Close, or by a GC cleanup once the
// Instance is unreachable.
func (m *mmapMemory) Free() {}

// release unmaps the memory. It must only run when no guest code of the
// owning Instance can be executing.
func (m *mmapMemory) release() {
	m.once.Do(func() {
		_ = sysMunmap(m.reserved)
		m.reserved = nil
	})
}

func pageAlignUp(n uint64) uint64 {
	ps := uint64(syscall.Getpagesize())
	return (n + ps - 1) &^ (ps - 1)
}
