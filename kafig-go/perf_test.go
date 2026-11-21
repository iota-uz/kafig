package kafig

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"
)

// TestFreshInstanceLatency measures the per-run cost of the "fresh instance
// per execution" pattern: Runtime.Instance + Eval + Close, with a deadline on
// the context as in production. CI runs it on linux/amd64 with
// KAFIG_PERF=1 KAFIG_PERF_MAX_P50=500us; the README records the numbers.
func TestFreshInstanceLatency(t *testing.T) {
	if os.Getenv("KAFIG_PERF") == "" {
		t.Skip("set KAFIG_PERF=1 to run")
	}
	ctx := context.Background()
	rt, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(ctx)
	router := noopRouter()

	run := func() time.Duration {
		start := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		inst, err := rt.Instance(callCtx, WithRouter(router))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := inst.Eval(callCtx, `({ sum: [1, 2, 3].reduce((a, b) => a + b, 0) })`); err != nil {
			t.Fatal(err)
		}
		if err := inst.Close(callCtx); err != nil {
			t.Fatal(err)
		}
		cancel()
		return time.Since(start)
	}

	for i := 0; i < 300; i++ {
		run()
	}
	const n = 3000
	samples := make([]time.Duration, n)
	for i := range samples {
		samples[i] = run()
	}
	sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })
	pct := func(p float64) time.Duration { return samples[int(p*float64(n-1))] }
	p50 := pct(0.50)
	t.Logf("fresh instance (create + eval + close), %d runs: p50=%v p90=%v p99=%v max=%v",
		n, p50, pct(0.90), pct(0.99), samples[n-1])

	if limit := os.Getenv("KAFIG_PERF_MAX_P50"); limit != "" {
		max, err := time.ParseDuration(limit)
		if err != nil {
			t.Fatalf("KAFIG_PERF_MAX_P50: %v", err)
		}
		if p50 > max {
			t.Fatalf("p50 %v exceeds %v", p50, max)
		}
	}
}

func BenchmarkFreshInstance(b *testing.B) {
	ctx := context.Background()
	rt, err := New(ctx, WithCompilationCache(testCache))
	if err != nil {
		b.Fatal(err)
	}
	defer rt.Close(ctx)
	router := noopRouter()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		inst, err := rt.Instance(ctx, WithRouter(router))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := inst.Eval(ctx, `1 + 1`); err != nil {
			b.Fatal(err)
		}
		inst.Close(ctx)
	}
}
