package kafig

// Isolation suite: what untrusted JavaScript must NOT be able to do, and how
// the sandbox must fail when limits are hit. Runs in CI on linux/amd64 and
// darwin/arm64 (see .github/workflows/ci.yml).

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// deadlineSlack is how far past the context deadline an Eval may return.
const deadlineSlack = 50 * time.Millisecond

func newIsolationInstance(t *testing.T, rtOpts []RuntimeOption, instOpts ...InstanceOption) (*Runtime, *Instance) {
	t.Helper()
	rt, err := New(context.Background(), append([]RuntimeOption{WithCompilationCache(testCache)}, rtOpts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	if len(instOpts) == 0 {
		instOpts = []InstanceOption{WithRouter(noopRouter())}
	}
	inst, err := rt.Instance(context.Background(), instOpts...)
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	t.Cleanup(func() { inst.Close(context.Background()) })
	return rt, inst
}

func evalString(t *testing.T, inst *Instance, src string, opts ...EvalOption) string {
	t.Helper()
	raw, err := inst.Eval(context.Background(), src, opts...)
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("Eval(%q) = %s, want a JSON string", src, raw)
	}
	return s
}

func wantRuntimeError(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("got error %v (%T), want *RuntimeError with code %q", err, err, code)
	}
	if rtErr.Code != code {
		t.Fatalf("got code %q (%v), want %q", rtErr.Code, rtErr, code)
	}
}

// ─── No ambient authority ───────────────────────────────────────────────────

// The global object holds exactly the ECMAScript built-ins QuickJS-ng ships
// plus kafig's host API. Anything new (a QuickJS upgrade adding std/os, a
// leaked native bridge) fails this test and has to be reviewed.
func TestIsolationGlobalObjectAllowlist(t *testing.T) {
	_, inst := newIsolationInstance(t, nil)

	got := strings.Split(evalString(t, inst, `Object.getOwnPropertyNames(globalThis).sort().join(",")`), ",")
	want := []string{
		// ECMAScript built-ins
		"AggregateError", "Array", "ArrayBuffer", "BigInt", "BigInt64Array", "BigUint64Array",
		"Boolean", "DOMException", "DataView", "Date", "Error", "EvalError", "FinalizationRegistry",
		"Float16Array", "Float32Array", "Float64Array", "Function", "Infinity", "Int16Array",
		"Int32Array", "Int8Array", "InternalError", "Iterator", "JSON", "Map", "Math", "NaN",
		"Number", "Object", "Promise", "Proxy", "RangeError", "ReferenceError", "Reflect", "RegExp",
		"Set", "SharedArrayBuffer", "String", "Symbol", "SyntaxError", "TypeError", "URIError",
		"Uint16Array", "Uint32Array", "Uint8Array", "Uint8ClampedArray", "WeakMap", "WeakRef",
		"WeakSet", "decodeURI", "decodeURIComponent", "encodeURI", "encodeURIComponent", "escape",
		"eval", "globalThis", "isFinite", "isNaN", "parseFloat", "parseInt", "performance",
		"queueMicrotask", "undefined", "unescape",
		// kafig host API and frozen prelude entry points
		"__dispatch_event", "__reject_rpc", "__reset", "__resolve_rpc", "host",
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("global object changed\n got: %v\nwant: %v", got, want)
	}

	for _, name := range []string{
		"std", "os", "print", "console", "scriptArgs", "require", "process", "Deno", "fetch",
		"XMLHttpRequest", "WebAssembly", "Worker", "__host_rpc_native", "__host_rpc_sync_native",
	} {
		if typ := evalString(t, inst, `typeof `+name); typ != "undefined" {
			t.Errorf("typeof %s = %q, want undefined", name, typ)
		}
	}
}

// import() cannot load anything: there is no module loader, so neither the
// QuickJS std/os modules nor files nor data: URLs resolve.
func TestIsolationDynamicImportBlocked(t *testing.T) {
	_, inst := newIsolationInstance(t, nil)

	for _, spec := range []string{
		"std", "os", "qjs:std", "qjs:os", "/etc/hosts", "./x.js", "../x.js", "x.js",
		"file:///etc/passwd", "data:text/javascript,export default 1",
	} {
		src := `await import(` + jsString(spec) + `).then(() => "loaded", e => "blocked: " + e)`
		if got := evalString(t, inst, src, WithAsync()); !strings.HasPrefix(got, "blocked:") {
			t.Errorf("import(%q) = %q, want it blocked", spec, got)
		}
	}

	// Static import is not even syntax in a global script.
	if _, err := inst.Eval(context.Background(), `import * as std from "std"`); err == nil {
		t.Errorf("static import evaluated without error")
	}
}

//go:embed testdata/wasi_probe.wasm
var wasiProbeWasm []byte

// The WASI configuration given to every Instance exposes no filesystem,
// environment or arguments, and guest output never reaches the host's
// stdout/stderr. Checked with a probe module (testdata/wasi_probe.wat)
// instantiated with the very ModuleConfig kafig uses.
func TestIsolationWASIHasNoFilesystemEnvOrStdio(t *testing.T) {
	t.Setenv("KAFIG_ISOLATION_SECRET", "must-not-leak")
	ctx := context.Background()

	rt, err := New(ctx, WithCompilationCache(testCache))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(ctx)

	wz := wazero.NewRuntime(ctx)
	defer wz.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, wz)

	restore := captureStdio(t)
	mod, err := wz.InstantiateWithConfig(ctx, wasiProbeWasm, rt.moduleConfig())
	if err != nil {
		restore()
		t.Fatal(err)
	}
	call := func(name string, args ...uint64) uint32 {
		t.Helper()
		res, err := mod.ExportedFunction(name).Call(ctx, args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return uint32(res[0])
	}
	const ebadf = 8
	for fd := uint64(3); fd < 8; fd++ {
		if errno := call("prestat", fd); errno != ebadf {
			t.Errorf("fd_prestat_get(%d) errno = %d, want EBADF: a directory is preopened", fd, errno)
		}
	}
	if errno := call("open_hosts", 3); errno == 0 {
		t.Errorf("path_open(3, etc/hosts) succeeded")
	}
	if n := call("env_count"); n != 0 {
		t.Errorf("guest sees %d environment variables, want 0", n)
	}
	if n := call("args_count"); n != 0 {
		t.Errorf("guest sees %d arguments, want 0", n)
	}
	call("write_fd", 1)
	call("write_fd", 2)
	if out := restore(); out != "" {
		t.Errorf("guest output reached host stdio: %q", out)
	}
}

// captureStdio redirects os.Stdout/os.Stderr into a pipe; the returned func
// restores them and returns what was written.
func captureStdio(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	var restored bool
	return func() string {
		if restored {
			return ""
		}
		restored = true
		os.Stdout, os.Stderr = oldOut, oldErr
		w.Close()
		return <-done
	}
}

// ─── Prelude internals ──────────────────────────────────────────────────────

func TestIsolationPreludeInternalsFrozen(t *testing.T) {
	_, inst := newIsolationInstance(t, nil)

	for _, name := range []string{"__dispatch_event", "__reject_rpc", "__reset", "__resolve_rpc"} {
		desc := evalString(t, inst, `(() => {
			const d = Object.getOwnPropertyDescriptor(globalThis, "`+name+`");
			return [typeof d.value, d.writable, d.configurable].join(",");
		})()`)
		if desc != "function,false,false" {
			t.Errorf("%s descriptor = %s, want function,false,false", name, desc)
		}
		got := evalString(t, inst, `(() => {
			const before = globalThis.`+name+`;
			const results = [];
			try { globalThis.`+name+` = () => "pwned"; results.push("assigned"); } catch (e) { results.push(e.name); }
			try { results.push(String(delete globalThis.`+name+`)); } catch (e) { results.push(e.name); }
			try { Object.defineProperty(globalThis, "`+name+`", { value: 1 }); results.push("redefined"); } catch (e) { results.push(e.name); }
			results.push(String(globalThis.`+name+` === before));
			return results.join(",");
		})()`)
		if got != "TypeError,TypeError,TypeError,true" {
			t.Errorf("tampering with %s: %s, want TypeError,TypeError,TypeError,true", name, got)
		}
	}

	got := evalString(t, inst, `(() => {
		const r = [String(Object.isFrozen(host))];
		try { host.rpc = () => "pwned"; r.push("assigned"); } catch (e) { r.push(e.name); }
		try { globalThis.host = {}; r.push("replaced"); } catch (e) { r.push(e.name); }
		return r.join(",");
	})()`)
	// host itself is a plain (writable) global binding, but the object is
	// frozen: user code may shadow it for itself, never alter the original.
	if !strings.HasPrefix(got, "true,TypeError") {
		t.Errorf("host object not frozen: %s", got)
	}
}

// ─── Memory ─────────────────────────────────────────────────────────────────

func TestIsolationMemoryLimitIsCleanError(t *testing.T) {
	const grow = `(() => { const keep = []; for (;;) keep.push(new Array(1e5).fill(Math.random())); })()`
	cases := []struct {
		name   string
		rtOpts []RuntimeOption
		inst   []InstanceOption
	}{
		// The WASM page cap (16 MiB) is reached before QuickJS's own limit.
		{"page cap/mmap", []RuntimeOption{WithWASMMemoryLimitPages(256)}, []InstanceOption{WithJSMemoryLimit(256 << 20)}},
		{"page cap/heap", []RuntimeOption{WithWASMMemoryLimitPages(256), WithMmapMemory(false)}, []InstanceOption{WithJSMemoryLimit(256 << 20)}},
		{"page cap/heap+capacity-from-max", []RuntimeOption{WithWASMMemoryLimitPages(256), WithMmapMemory(false), WithMemoryCapacityFromMax(true)}, []InstanceOption{WithJSMemoryLimit(256 << 20)}},
		// QuickJS's heap limit (8 MiB) is reached first.
		{"js limit", []RuntimeOption{WithWASMMemoryLimitPages(1024)}, []InstanceOption{WithJSMemoryLimit(8 << 20)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, inst := newIsolationInstance(t, tc.rtOpts, append([]InstanceOption{WithRouter(noopRouter())}, tc.inst...)...)
			_, err := inst.Eval(context.Background(), grow)
			wantRuntimeError(t, err, ErrorCodeMemoryLimitExceeded)

			// The instance survives and the memory is reclaimable.
			if _, err := inst.Eval(context.Background(), `1 + 1`); err != nil {
				t.Fatalf("instance unusable after OOM: %v", err)
			}
		})
	}
}

// ─── Stack ──────────────────────────────────────────────────────────────────

func TestIsolationStackOverflowIsError(t *testing.T) {
	_, inst := newIsolationInstance(t, nil)

	for _, src := range []string{
		`(function f() { return f() + 1; })()`,
		`JSON.parse("[".repeat(1e6))`,
		`(() => { let a = []; for (let i = 0; i < 1e5; i++) a = [a]; return String(a); })()`,
		`(() => { const p = new Proxy({}, { get(t, k, r) { return r[k]; } }); return p.x; })()`,
	} {
		_, err := inst.Eval(context.Background(), src)
		wantRuntimeError(t, err, ErrorCodeStackOverflow)
	}

	// Caught in JS like any RangeError, and the instance keeps working.
	got := evalString(t, inst, `(() => { try { (function f() { f(); })(); } catch (e) { return e.name; } })()`)
	if got != "RangeError" {
		t.Errorf("caught %q, want RangeError", got)
	}
	if _, err := inst.Eval(context.Background(), `1 + 1`); err != nil {
		t.Fatalf("instance unusable after stack overflow: %v", err)
	}
}

// ─── Deadline ───────────────────────────────────────────────────────────────

// Every way of keeping the sandbox busy must end within deadline + 50 ms,
// both with close-on-context-done (default) and with only the cooperative
// interrupt.
func TestIsolationDeadlineStopsExecution(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	block := func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		<-release // ignores ctx on purpose: the worst-behaved handler
		return json.RawMessage(`null`), nil
	}
	router := func() *RPCRouter {
		return NewRPCRouter().WithAsync("hang", block).WithSync("hangSync", block)
	}

	cases := []struct {
		name            string
		src             string
		async           bool
		cooperativeOnly bool // also stopped without close-on-context-done
	}{
		{"busy loop", `for (;;) {}`, false, true},
		{"busy loop in promise jobs", `await (async () => { for (;;) await null; })()`, true, true},
		{"ReDoS", `/^(a+)+$/.test("a".repeat(40) + "!")`, false, true},
		{"hanging async host call", `await host.rpc("hang")`, true, true},
		{"hanging sync host call", `host.rpcSync("hangSync")`, false, true},
		{"sync host call swallowed, then loop", `try { host.rpcSync("hangSync") } catch {} for (;;) {}`, false, true},
		{"huge BigInt power", `(7n ** 9999999n) % 10n`, false, false},
	}
	modes := []struct {
		name        string
		cooperative bool
	}{{"close-on-done", false}, {"cooperative", true}}

	for _, mode := range modes {
		for _, tc := range cases {
			if mode.cooperative && !tc.cooperativeOnly {
				continue
			}
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				var rtOpts []RuntimeOption
				if mode.cooperative {
					rtOpts = append(rtOpts, WithCloseOnContextDone(false))
				}
				_, inst := newIsolationInstance(t, rtOpts, WithRouter(router()))

				const deadline = 100 * time.Millisecond
				ctx, cancel := context.WithTimeout(context.Background(), deadline)
				defer cancel()
				var opts []EvalOption
				if tc.async {
					opts = append(opts, WithAsync())
				}
				start := time.Now()
				_, err := inst.Eval(ctx, tc.src, opts...)
				elapsed := time.Since(start)

				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v, want context.DeadlineExceeded", err)
				}
				wantRuntimeError(t, err, ErrorCodeDeadlineExceeded)
				if elapsed > deadline+deadlineSlack {
					t.Fatalf("returned after %v, want <= %v", elapsed, deadline+deadlineSlack)
				}
			})
		}
	}
}

// Acceptance: 100 runs of "fetch a slow server while the deadline expires"
// never hang. The handler deliberately ignores ctx (worst case).
func TestIsolationSlowFetchDeadline100Runs(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		w.Write([]byte(`"late"`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: unblock handlers so Close returns

	client := &http.Client{}
	router := NewRPCRouter().WithAsync("fetch", func(_ context.Context, params json.RawMessage) (json.RawMessage, error) {
		var url string
		if err := json.Unmarshal(params, &url); err != nil {
			return nil, err
		}
		resp, err := client.Get(url) // no ctx: ignores the deadline
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return json.RawMessage(b), err
	})

	rt, err := New(context.Background(), WithCompilationCache(testCache))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(context.Background())

	const deadline = 30 * time.Millisecond
	var worst time.Duration
	for i := 0; i < 100; i++ {
		inst, err := rt.Instance(context.Background(), WithRouter(router))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		start := time.Now()
		_, err = inst.Eval(ctx, `await host.rpc("fetch", `+jsString(srv.URL)+`)`, WithAsync())
		elapsed := time.Since(start)
		cancel()
		inst.Close(context.Background())

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("run %d: err = %v, want deadline exceeded", i, err)
		}
		if elapsed > deadline+deadlineSlack {
			t.Fatalf("run %d: returned after %v, want <= %v", i, elapsed, deadline+deadlineSlack)
		}
		worst = max(worst, elapsed)
	}
	t.Logf("100 runs, worst return time %v (deadline %v)", worst, deadline)
}

// ─── Interrupt while draining the job queue ─────────────────────────────────

func TestIsolationInterruptInJobQueueIsError(t *testing.T) {
	t.Run("interrupt callback", func(t *testing.T) {
		_, inst := newIsolationInstance(t, nil,
			WithRouter(noopRouter()),
			WithInterruptCallback(func(opcodes, _ uint64) bool { return opcodes > 2_000_000 }))
		res, err := inst.Eval(context.Background(), `await (async () => { for (;;) await null; })()`, WithAsync())
		if err == nil {
			t.Fatalf("got empty success (%s), want cpu_limit_exceeded", res)
		}
		wantRuntimeError(t, err, ErrorCodeCPULimitExceeded)
	})

	t.Run("Interrupt from another goroutine", func(t *testing.T) {
		_, inst := newIsolationInstance(t, nil)
		timer := time.AfterFunc(50*time.Millisecond, inst.Interrupt)
		defer timer.Stop()
		res, err := inst.Eval(context.Background(), `(async () => { for (;;) await null; })(); 1`, WithAsync())
		if err == nil {
			t.Fatalf("got empty success (%s), want cpu_limit_exceeded", res)
		}
		wantRuntimeError(t, err, ErrorCodeCPULimitExceeded)
	})

	t.Run("instance usable afterwards", func(t *testing.T) {
		var stop atomic.Bool
		stop.Store(true)
		_, inst := newIsolationInstance(t, nil,
			WithRouter(noopRouter()),
			WithInterruptCallback(func(uint64, uint64) bool { return stop.Load() }))
		_, err := inst.Eval(context.Background(), `(async () => { for (;;) await null; })()`, WithAsync())
		wantRuntimeError(t, err, ErrorCodeCPULimitExceeded)
		stop.Store(false)
		if got := evalString(t, inst, `await Promise.resolve("ok")`, WithAsync()); got != "ok" {
			t.Fatalf("after interrupt: %q", got)
		}
	})
}

// ─── Clock and randomness ───────────────────────────────────────────────────

func TestIsolationDateNowIsWallClock(t *testing.T) {
	_, inst := newIsolationInstance(t, nil)
	raw, err := inst.Eval(context.Background(), `Date.now()`)
	if err != nil {
		t.Fatal(err)
	}
	var ms float64
	if err := json.Unmarshal(raw, &ms); err != nil {
		t.Fatal(err)
	}
	host := float64(time.Now().UnixMilli())
	if d := math.Abs(host - ms); d > 1000 {
		t.Fatalf("Date.now() = %v, host = %v: off by %v ms (> 1 s)", ms, host, d)
	}
	// new Date() follows the same clock.
	year := evalString(t, inst, `String(new Date().getUTCFullYear())`)
	if year != time.Now().UTC().Format("2006") {
		t.Fatalf("new Date() year = %s", year)
	}
}

func TestIsolationMathRandomPerInstance(t *testing.T) {
	rt, err := New(context.Background(), WithCompilationCache(testCache))
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(context.Background())

	seq := func(rt *Runtime) string {
		inst, err := rt.Instance(context.Background(), WithRouter(noopRouter()))
		if err != nil {
			t.Fatal(err)
		}
		defer inst.Close(context.Background())
		raw, err := inst.Eval(context.Background(), `[Math.random(), Math.random(), Math.random()]`)
		if err != nil {
			t.Fatal(err)
		}
		var xs []float64
		if err := json.Unmarshal(raw, &xs); err != nil {
			t.Fatal(err)
		}
		for _, x := range xs {
			if x < 0 || x >= 1 {
				t.Fatalf("Math.random() = %v out of [0,1)", x)
			}
		}
		if xs[0] == xs[1] && xs[1] == xs[2] {
			t.Fatalf("Math.random() is constant: %v", xs)
		}
		return string(raw)
	}
	a, b := seq(rt), seq(rt)
	if a == b {
		t.Fatalf("two instances produced the same Math.random sequence %s", a)
	}

	// The sequence comes from the configured entropy source.
	det := func() *Runtime {
		rt, err := New(context.Background(), WithCompilationCache(testCache), WithRandSource(zeroReader{}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rt.Close(context.Background()) })
		return rt
	}
	if x, y := seq(det()), seq(det()); x != y {
		t.Fatalf("deterministic rand source gave %s and %s", x, y)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// ─── Bytecode ───────────────────────────────────────────────────────────────

func TestIsolationRejectsForeignBytecode(t *testing.T) {
	rt, inst := newIsolationInstance(t, nil)
	bc, err := rt.Compile(context.Background(), `40 + 2`)
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := inst.EvalCompiled(context.Background(), bc); err != nil || string(raw) != "42" {
		t.Fatalf("own bytecode: %s, %v", raw, err)
	}

	tampered := append([]byte(nil), bc...)
	tampered[len(tampered)/3] ^= 0xff
	_, other := newIsolationInstance(t, nil)
	for name, input := range map[string][]byte{
		"tampered":      tampered,
		"truncated":     bc[:len(bc)-1],
		"raw (no tag)":  bc[:len(bc)-bytecodeTagSize],
		"empty":         nil,
		"other runtime": bc,
	} {
		target := inst
		if name == "other runtime" {
			target = other
		}
		if _, err := target.EvalCompiled(context.Background(), input); !errors.Is(err, ErrForeignBytecode) {
			t.Errorf("%s: err = %v, want ErrForeignBytecode", name, err)
		}
	}
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Compile (and the prelude compilation in New) runs on a temporary wazero
// runtime; it must be bound by the same page cap as Instances.
func TestIsolationCompileRespectsPageCap(t *testing.T) {
	big := `"` + strings.Repeat("a", 6<<20) + `".length`

	uncapped, err := New(context.Background(), WithCompilationCache(testCache))
	if err != nil {
		t.Fatal(err)
	}
	defer uncapped.Close(context.Background())
	if _, err := uncapped.Compile(context.Background(), big); err != nil {
		t.Fatalf("uncapped Compile: %v", err)
	}

	capped, err := New(context.Background(), WithCompilationCache(testCache), WithWASMMemoryLimitPages(64)) // 4 MiB
	if err != nil {
		t.Fatal(err)
	}
	defer capped.Close(context.Background())
	if _, err := capped.Compile(context.Background(), big); err == nil {
		t.Fatal("Compile of a 6 MiB source succeeded under a 4 MiB page cap")
	}
	// Small sources still compile under the cap.
	if _, err := capped.Compile(context.Background(), `1 + 1`); err != nil {
		t.Fatalf("small Compile under cap: %v", err)
	}

	// The prelude is compiled the same way.
	if _, err := New(context.Background(), WithCompilationCache(testCache),
		WithWASMMemoryLimitPages(64), WithPrelude(big)); err == nil {
		t.Fatal("New with a 6 MiB prelude succeeded under a 4 MiB page cap")
	}
}
