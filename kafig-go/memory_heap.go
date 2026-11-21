package kafig

// heapMemory is a Go-heap linear memory bounded by a maximum size. It is the
// fallback of the mmap allocator when a mapping cannot be created; it
// behaves like wazero's default allocator (growth copies, the GC frees).
type heapMemory struct {
	buf      []byte
	maxBytes uint64
}

func newHeapMemory(capacity, maxBytes uint64) *heapMemory {
	return &heapMemory{buf: make([]byte, 0, capacity), maxBytes: maxBytes}
}

func (h *heapMemory) Reallocate(size uint64) []byte {
	if size > h.maxBytes {
		return nil
	}
	if size <= uint64(cap(h.buf)) {
		h.buf = h.buf[:size]
		return h.buf
	}
	grown := make([]byte, size, max(size, min(2*uint64(cap(h.buf)), h.maxBytes)))
	copy(grown, h.buf)
	h.buf = grown
	return h.buf
}

func (h *heapMemory) Free() { h.buf = nil }
