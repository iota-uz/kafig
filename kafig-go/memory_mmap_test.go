//go:build (linux || darwin) && (amd64 || arm64)

package kafig

import (
	"context"
	"errors"
	"sync/atomic"
	"syscall"
	"testing"
)

// failSyscalls swaps the allocator's system calls for the duration of a test.
func failSyscalls(t *testing.T, mmap func(int, int64, int, int, int) ([]byte, error), mprotect func([]byte, int) error) {
	t.Helper()
	oldMmap, oldMprotect := sysMmap, sysMprotect
	if mmap != nil {
		sysMmap = mmap
	}
	if mprotect != nil {
		sysMprotect = mprotect
	}
	t.Cleanup(func() { sysMmap, sysMprotect = oldMmap, oldMprotect })
}

func TestMmapAllocatorFallsBackWhenMappingFails(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"mmap fails": func(t *testing.T) {
			failSyscalls(t, func(int, int64, int, int, int) ([]byte, error) { return nil, syscall.ENOMEM }, nil)
		},
		"initial mprotect fails": func(t *testing.T) {
			failSyscalls(t, nil, func([]byte, int) error { return syscall.ENOMEM })
		},
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			rt, err := New(context.Background(), WithCompilationCache(testCache), WithWASMMemoryLimitPages(256))
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close(context.Background())
			inject(t)

			inst, err := rt.Instance(context.Background(), WithRouter(noopRouter()))
			if err != nil {
				t.Fatalf("Instance: %v", err)
			}
			defer inst.Close(context.Background())
			if inst.memory != nil {
				t.Fatalf("expected the Go-heap fallback, got an mmap-backed memory")
			}
			if raw, err := inst.Eval(context.Background(), `"x".repeat(1 << 20).length`); err != nil || string(raw) != "1048576" {
				t.Fatalf("Eval on fallback memory: %s, %v", raw, err)
			}
			// The fallback keeps the page cap.
			_, err = inst.Eval(context.Background(), `(() => { const k = []; for (;;) k.push("y".repeat(1 << 16) + k.length); })()`)
			wantRuntimeError(t, err, ErrorCodeMemoryLimitExceeded)
		})
	}
}

// A failed mprotect while the guest grows its memory is a failed memory.grow
// (out of memory) for the guest, never a host panic.
func TestMmapGrowFailureIsOutOfMemory(t *testing.T) {
	rt, err := New(context.Background(), WithCompilationCache(testCache))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(context.Background())
	inst, err := rt.Instance(context.Background(), WithRouter(noopRouter()), WithJSMemoryLimit(1<<30))
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close(context.Background())
	if inst.memory == nil {
		t.Fatal("expected an mmap-backed memory")
	}

	var calls atomic.Int32
	failSyscalls(t, nil, func(b []byte, prot int) error {
		calls.Add(1)
		return errors.New("injected mprotect failure")
	})
	_, err = inst.Eval(context.Background(), `(() => { const k = []; for (;;) k.push("z".repeat(1 << 16) + k.length); })()`)
	wantRuntimeError(t, err, ErrorCodeMemoryLimitExceeded)
	if calls.Load() == 0 {
		t.Fatal("growth never reached mprotect")
	}
}
