//go:build linux || darwin

package kafig

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestPeakRSSAtOOM measures the process's peak RSS while instances run into
// the WASM page cap, for each linear-memory strategy. Each configuration runs
// in a fresh child process so peaks do not mix. Run with KAFIG_RSS=1; the
// README records the results and the resulting defaults.
func TestPeakRSSAtOOM(t *testing.T) {
	if os.Getenv("KAFIG_RSS") == "" {
		t.Skip("set KAFIG_RSS=1 to run")
	}
	configs := []string{"mmap", "heap", "heap+capacity-from-max"}
	t.Logf("| config | instances | page cap | baseline RSS | peak RSS | peak - baseline | time to OOM |")
	t.Logf("|---|---|---|---|---|---|---|")
	for _, cfg := range configs {
		for _, n := range []int{1, 8} {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRSSChild$", "-test.v")
			cmd.Env = append(os.Environ(), "KAFIG_RSS_CHILD="+cfg, fmt.Sprintf("KAFIG_RSS_INSTANCES=%d", n))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", cfg, err, out)
			}
			var res rssResult
			found := false
			sc := bufio.NewScanner(strings.NewReader(string(out)))
			for sc.Scan() {
				if line, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "RSS_RESULT "); ok {
					if err := json.Unmarshal([]byte(line), &res); err != nil {
						t.Fatal(err)
					}
					found = true
				}
			}
			if !found {
				t.Fatalf("%s: no result\n%s", cfg, out)
			}
			t.Logf("| %s | %d | %d MiB | %.1f MiB | %.1f MiB | %.1f MiB | %v |", cfg, n, res.CapMiB,
				res.BaselineMiB, res.PeakMiB, res.PeakMiB-res.BaselineMiB, res.Elapsed.Round(time.Millisecond))
		}
	}
}

type rssResult struct {
	CapMiB      int
	BaselineMiB float64
	PeakMiB     float64
	Elapsed     time.Duration
}

func TestRSSChild(t *testing.T) {
	cfg := os.Getenv("KAFIG_RSS_CHILD")
	if cfg == "" {
		t.Skip("helper for TestPeakRSSAtOOM")
	}
	var instances int
	fmt.Sscan(os.Getenv("KAFIG_RSS_INSTANCES"), &instances)

	const capPages = 2048 // 128 MiB
	opts := []RuntimeOption{WithWASMMemoryLimitPages(capPages)}
	switch cfg {
	case "mmap":
		opts = append(opts, WithMmapMemory(true))
	case "heap":
		opts = append(opts, WithMmapMemory(false))
	case "heap+capacity-from-max":
		opts = append(opts, WithMmapMemory(false), WithMemoryCapacityFromMax(true))
	default:
		t.Fatalf("unknown config %q", cfg)
	}
	ctx := context.Background()
	rt, err := New(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(ctx)
	runtime.GC()
	baseline := maxRSSMiB(t)

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, instances)
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inst, err := rt.Instance(ctx, WithRouter(noopRouter()), WithJSMemoryLimit(1<<30))
			if err != nil {
				errs <- err
				return
			}
			defer inst.Close(ctx)
			_, err = inst.Eval(ctx, `(() => { const keep = []; for (;;) keep.push("x".repeat(1 << 16) + keep.length); })()`)
			var rtErr *RuntimeError
			if !errors.As(err, &rtErr) || rtErr.Code != ErrorCodeMemoryLimitExceeded {
				errs <- fmt.Errorf("want memory_limit_exceeded, got %v", err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rssResult{CapMiB: capPages / 16, BaselineMiB: baseline, PeakMiB: maxRSSMiB(t), Elapsed: time.Since(start)})
	fmt.Printf("RSS_RESULT %s\n", b)
}

func maxRSSMiB(t *testing.T) float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		return float64(ru.Maxrss) / (1 << 20) // bytes
	}
	return float64(ru.Maxrss) / 1024 // KiB
}
