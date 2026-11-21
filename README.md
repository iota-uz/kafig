<div align="center">
  <img src="./kafig.webp" alt="Kafig" width="200" />
</div>

# {JS} Kafig

> **This is the iota-uz fork of [merlinfuchs/kafig](https://github.com/merlinfuchs/kafig)**
> (MIT, original authorship preserved in the git history). It adds a
> reproducible, checksum-verified `runtime.wasm` build, sandbox hardening
> patches, an isolation test suite and semver tags for the Go module. See
> [Fork changes](#fork-changes) below.

Kafig (from German _Käfig_, meaning cage) is a sandboxed JavaScript runtime for Go. It lets you execute untrusted JavaScript with strict CPU and memory limits, full async/await support, and a bidirectional RPC bridge between JS and Go.

The runtime compiles [QuickJS-ng](https://github.com/quickjs-ng/quickjs) to WebAssembly via Rust, then executes it inside [wazero](https://wazero.io/) (a pure-Go WASM runtime). Each instance gets its own isolated linear memory and JS heap with no shared state.

## Architecture

```
Go application
  └─ kafig-go          ← Go API: Runtime, Instance, RPCRouter
       └─ wazero       ← pure-Go WASM host, enforces sandbox
            └─ kafig-runtime.wasm
                 └─ QuickJS (compiled from Rust via rquickjs)
```

**Key properties:**

- Each Instance has its own WASM linear memory and QuickJS heap (configurable limits)
- CPU time is tracked per-execution and excludes time spent waiting for RPC results
- JS can call Go functions via async (`host.rpc`) or sync (`host.rpcSync`) RPC
- Go can call into JS via named event handlers registered with `host.on`
- The WASM binary is pre-initialized with [Wizer](https://github.com/bytecodealliance/wizer). QuickJS and the JS prelude are snapshotted at build time, so instantiation has near-zero startup cost
- JavaScript can be pre-compiled to QuickJS bytecode, skipping parse overhead on repeated evaluations

## Usage

```shell
go get github.com/iota-uz/kafig/kafig-go@v0.3.1
```

The module requires Go 1.24+ and wazero 1.11+.

### Create a Runtime and Instance

```go
rt, err := kafig.New(ctx,
    kafig.WithPrelude(`const VERSION = "1.0";`),
    kafig.WithWASMMemoryLimitPages(512), // 32 MB WASM linear memory per instance
)
defer rt.Close(ctx)

router := kafig.NewRPCRouter().
    WithAsync("fetch", func(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
        // runs in a goroutine, suitable for I/O
        var req struct{ URL string }
        json.Unmarshal(params, &req)
        resp, _ := http.Get(req.URL)
        // ...
        return json.Marshal(result)
    }).
    WithSync("now", func(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
        // runs inline, no goroutine. Suitable for fast, pure-compute calls
        return json.Marshal(map[string]int64{"ts": time.Now().Unix()})
    })

inst, err := rt.Instance(ctx,
    kafig.WithRouter(router),
    kafig.WithInterruptCallback(func(opcodes, cpuTimeUs uint64) bool {
        return cpuTimeUs > 5_000_000 // 5 second CPU limit
    }),
    kafig.WithPromiseRejectionHandler(func(err *kafig.JsError) bool {
        log.Printf("unhandled rejection: %s", err)
        return false // return true to interrupt execution
    }),
    kafig.WithJSMemoryLimit(16 * 1024 * 1024),  // 16 MB QuickJS heap
)
defer inst.Close(ctx)
```

### Evaluate JavaScript

```go
// Synchronous (no promises, no RPC)
result, err := inst.Eval(ctx, `1 + 2`)

// Async (enables top-level await, promise resolution and RPC processing)
result, err := inst.Eval(ctx, `
    const data = await host.rpc("fetch", {url: "https://api.example.com"});
    data
`, kafig.WithAsync())
```

### Event Handlers

Scripts can register persistent event handlers that Go dispatches later:

```go
// During eval, JS registers handlers:
inst.Eval(ctx, `
    host.on("process", async (params) => {
        const result = await host.rpc("fetch", {url: params.url});
        return result;
    });
`, kafig.WithAsync())

// Later, Go dispatches events:
result, err := inst.DispatchEvent(ctx, "process",
    json.RawMessage(`{"url": "https://example.com"}`),
    kafig.WithAsync(),
)
```

### Pre-compiled Bytecode

```go
bytecode, err := rt.Compile(ctx, `42`)

// Reuse across instances of the same Runtime, skips parsing
result, err := inst.EvalCompiled(ctx, bytecode, kafig.WithAsync())
```

Bytecode returned by `Compile` is sealed with an HMAC under a key private to
the `Runtime`; `EvalCompiled` rejects anything else with `ErrForeignBytecode`
(QuickJS does not validate bytecode, so crafted bytecode could corrupt the
interpreter). Treat it as an opaque in-process cache entry, never persist it,
and never accept bytecode from users — only source.

### Execution Stats

```go
stats, _ := inst.GetExecutionStats(ctx)
fmt.Printf("opcodes: %d, cpu: %d us\n", stats.Opcodes, stats.CPUTimeUs)
inst.ResetExecutionStats(ctx)
```

### Error Handling

Eval, EvalCompiled, and DispatchEvent return two distinct error types:

- **`*kafig.JsError`** — a JavaScript exception was thrown. Contains `Name` (e.g. `"TypeError"`), `Message`, and an optional `Stack` trace.
- **`*kafig.RuntimeError`** — a resource limit or internal error. Contains a `Code` (`cpu_limit_exceeded`, `memory_limit_exceeded`, `stack_overflow`, `deadline_exceeded`, `canceled`, `runtime_error`) and a `Message`. For `deadline_exceeded` / `canceled`, `errors.Is(err, context.DeadlineExceeded)` / `errors.Is(err, context.Canceled)` hold.

```go
result, err := inst.Eval(ctx, `null.foo`, kafig.WithAsync())
if err != nil {
    switch e := err.(type) {
    case *kafig.JsError:
        fmt.Printf("JS %s: %s\n", e.Name, e.Message) // JS TypeError: cannot read property 'foo' of null
        if e.Stack != nil {
            fmt.Println(*e.Stack)
        }
    case *kafig.RuntimeError:
        fmt.Printf("%s: %s\n", e.Code, e.Message) // cpu_limit_exceeded: interrupted
    }
}
```

### Unhandled Promise Rejections

By default, unhandled promise rejections are silently ignored. Use `WithPromiseRejectionHandler` to receive them:

```go
inst, err := rt.Instance(ctx,
    kafig.WithRouter(router),
    kafig.WithPromiseRejectionHandler(func(err *kafig.JsError) bool {
        log.Printf("unhandled rejection: %s: %s", err.Name, err.Message)
        return false // continue execution
    }),
)
```

The handler is called immediately when the rejection occurs. Return `true` to interrupt execution (the current eval will return a `RuntimeError` with code `cpu_limit_exceeded`), or `false` to let execution continue.

### Resource Limits

Each instance can be configured with memory and CPU limits:

```go
inst, err := rt.Instance(ctx,
    kafig.WithRouter(router),

    // QuickJS heap memory limit (default: 32 MB).
    // Controls how much memory JS objects, strings, and arrays can use.
    kafig.WithJSMemoryLimit(8 * 1024 * 1024), // 8 MB

    // CPU time limit via interrupt callback.
    kafig.WithInterruptCallback(func(opcodes, cpuTimeUs uint64) bool {
        return cpuTimeUs > 1_000_000 // 1 second
    }),
)
```

The WASM linear memory cap is a Runtime option (`kafig.WithWASMMemoryLimitPages(256)` = 16 MB) and applies to every instance.

The context passed to `Eval`, `EvalCompiled` and `DispatchEvent` is the hard stop: when it is done the call returns `deadline_exceeded` (or `canceled`) — the interrupt handler observes it, pending host calls are abandoned, and with `WithCloseOnContextDone` (default) wazero aborts even long native operations, closing the instance.

`WithJSMemoryLimit` controls the QuickJS heap — allocations beyond this limit cause a JS out-of-memory error. `WithWASMMemoryLimitPages` caps the total WASM linear memory, which includes the QuickJS heap, Rust runtime overhead, and the WASM stack. Set both for defense in depth: the JS limit gives clean error messages, while the WASM limit is a hard ceiling enforced by the WASM runtime.

## JavaScript API

Inside the sandbox, scripts have access to the `host` object:

| Function                       | Description                                                     |
| ------------------------------ | --------------------------------------------------------------- |
| `host.rpc(method, params)`     | Async RPC call to Go. Returns a Promise.                        |
| `host.rpcSync(method, params)` | Sync RPC call to Go. Blocks until the handler returns.          |
| `host.on(name, fn)`            | Register an event handler callable from Go via `DispatchEvent`. |

The result of an `Eval` call is the value of the last expression in the script (like a REPL). For `DispatchEvent`, it's the return value of the handler function. In async mode, if the result is a Promise, it's automatically awaited before being returned to Go.

## Sync vs Async Execution

By default, `Eval`, `EvalCompiled`, and `DispatchEvent` run synchronously: the JS source executes and returns immediately with no promise resolution or RPC processing. This is the fast path for simple expressions and pure computation. The return value is the result of the last expression.

To enable promise resolution and RPC processing, pass `kafig.WithAsync()`. This enables top-level `await` (via QuickJS's `JS_EVAL_FLAG_ASYNC`) and tells kafig to drain the microtask queue and service RPC calls in a loop until the script settles. If the last expression evaluates to a Promise, it's automatically awaited before the result is returned to Go.

```go
// Sync: no promises, no RPC. Fast and simple.
result, _ := inst.Eval(ctx, `1 + 2`)

// Async: enables top-level await, promise resolution and RPC processing.
result, _ := inst.Eval(ctx, `
    const data = await host.rpc("fetch", {url: "https://example.com"});
    data
`, kafig.WithAsync())
```

`host.rpcSync()` is the exception: it works in both modes because it calls the Go handler inline during WASM execution and returns the result directly, without promises.

RPC handlers registered via `RPCRouter.WithAsync()` run in goroutines, so multiple concurrent `host.rpc()` calls (e.g. from `Promise.all`) execute in parallel on the Go side. Handlers registered via `RPCRouter.WithSync()` run inline with no goroutine overhead when the context can never be done; with a cancellable context they run on a helper goroutine so a handler that ignores `ctx` cannot hold the sandbox past its deadline.

## CLI

The `kafig-cli` tool provides an interactive REPL and file execution mode:

```shell
go install github.com/iota-uz/kafig/kafig-cli@kafig-cli/v0.3.1
```

The CLI is its own module (tags `kafig-cli/vX.Y.Z`) and depends on a released
`kafig-go` version. From a checkout, `cd kafig-cli && go install .` builds it
against the local `kafig-go` through `go.work`.

```bash
# Interactive REPL
kafig-cli

# Execute a file
kafig-cli script.js

# With CPU limits
kafig-cli -max-cpu-ms 1000 -max-opcodes 100000 script.js

# JSON mode (stdin/stdout)
echo '{"eval": "1 + 2"}' | kafig-cli
```

In the REPL, use `.dispatch <name> <json>` to dispatch events, `.reset` to reset the instance, and `.stats` to view execution statistics.

## Building from Source

`runtime.wasm` is built from source in CI and committed together with
`kafig-go/internal/wasm/runtime.wasm.sha256`. See
[Reproducible build](#reproducible-build).

```bash
scripts/build-wasm.sh --install   # rebuild runtime.wasm into kafig-go
scripts/build-wasm.sh --check     # rebuild and verify against the committed file
cd kafig-go && go test ./...
```

## Design Decisions

**JSON as the wire format.** All data crossing the WASM boundary is UTF-8 JSON. This is simple, debuggable, and avoids the complexity of shared-memory serialization formats. The tradeoff is serialization overhead on large payloads, but for typical RPC parameters and results, JSON is fast enough and keeps the protocol straightforward.

**Minimal JS API.** The guest-side API is intentionally small: `host.rpc`, `host.rpcSync`, and `host.on`. There is no way to expose Go functions directly into the JS global scope or call JS functions from Go by name. Everything goes through RPC calls or event handlers. This keeps the core simple and gives library users full control over the API surface they expose to scripts. You build your own JS API (helper functions, domain-specific abstractions) on top of these primitives, either via the prelude or using a bundle step for user scripts.

**No module system.** There is no `import`, `require`, or ES module support; `import()` rejects every specifier (no module loader is installed). Scripts run as top-level code in global scope. This is deliberate: module resolution adds complexity, filesystem access requirements, and attack surface. If you need to compose scripts, use a bundler like [esbuild](https://esbuild.github.io/) to bundle them into a single file.

**Opcode-based CPU limiting.** CPU budgets are enforced via QuickJS's interrupt handler, which fires every ~10,000 opcodes. This means the budget is measured in actual JS execution time (microseconds), excluding time spent waiting for RPC results. A script that makes a slow HTTP call via `host.rpc` only burns CPU budget while JS is actively running.

**Pre-initialization with Wizer.** The WASM binary includes a full snapshot of an initialized QuickJS runtime. This means instantiation restores memory from the snapshot instead of re-parsing and evaluating the prelude. The cost is a larger binary; the benefit is near-zero cold start.

**Single-threaded, serial execution.** Each Instance is single-threaded. Only one eval or event dispatch runs at a time. The caller must serialize access. This avoids the need for internal locking and makes the execution model easy to reason about.

**WASM as the isolation boundary.** Rather than relying on QuickJS's own sandboxing (which has had CVEs), kafig runs the entire JS engine inside a WASM sandbox. WASM provides hardware-enforced memory isolation: guest code physically cannot access host memory. This is a defense-in-depth approach: even if QuickJS has a memory safety bug, the WASM sandbox contains it.

**QuickJS over V8.** Kafig uses QuickJS, not V8 or SpiderMonkey. QuickJS is small (~200 KB compiled), deterministic, and compiles cleanly to WASM. It lacks JIT compilation, so raw throughput is lower than V8, but it's far more predictable, embeddable, and doesn't require platform-specific assembly. For sandboxed scripting workloads (config evaluation, event handlers, data transformation), parsing and I/O dominate, and QuickJS is more than fast enough.

## Fork changes

### Isolation guarantees

Enforced by `kafig-go/isolation_test.go`, which CI runs on linux/amd64 and
darwin/arm64:

- **No ambient authority.** The global object is an exact allowlist of
  ECMAScript built-ins plus `host` and the frozen prelude entry points; there
  is no `std`, `os`, `print`, `console`, `require`, `fetch`, `WebAssembly`.
  `import()` of `std`, `os`, `/etc/hosts`, `./x.js`, `file:`/`data:` URLs is
  rejected.
- **No filesystem, environment, arguments or stdio** at the WASI level: no
  preopened directories, empty environment and argv, empty stdin, discarded
  stdout/stderr (verified with a probe module using the exact `ModuleConfig`).
- **Prelude internals** (`__resolve_rpc`, `__reject_rpc`, `__dispatch_event`,
  `__reset`) are non-writable and non-configurable; `host` is frozen.
- **Memory**: hitting the WASM page cap or the QuickJS heap limit returns
  `memory_limit_exceeded`; the instance stays usable.
- **Deadline**: busy loops, loops in the promise job queue, ReDoS,
  hanging async and sync host calls and huge BigInt arithmetic stop within
  deadline + 50 ms, returning `deadline_exceeded`. 100 runs of "fetch a slow
  server while the deadline expires" (with a handler that ignores `ctx`) never
  hang.
- **Stack overflow** (recursion, `JSON.parse`, nested `toString`, Proxy
  traps) is a catchable `RangeError` / `stack_overflow`, not a trap. QuickJS-ng
  disables its stack check on WASI; `kafig-runtime/patches/rquickjs-sys`
  re-enables it inside the 1 MiB shadow stack (JS recursion depth ≈ 2.7k).
- **Interrupts while draining the job queue** (`for (;;) await null`) return
  `cpu_limit_exceeded`, never an empty success.
- **Clock and entropy**: `Date.now()` is the host wall clock (it was frozen at
  2022-01-01 by wazero's default); `Math.random` is seeded per instance from
  `crypto/rand` (QuickJS's seed was baked into the Wizer snapshot, so every
  instance replayed the same sequence). `WithRandSource` overrides it.
- **Bytecode**: only bytecode sealed by the same `Runtime.Compile` is accepted.

### Defaults

| Option | Upstream | This fork | Why |
|---|---|---|---|
| `WithCloseOnContextDone` | off | **on** | hard stop for native work that never reaches the interrupt check (e.g. `7n ** 9999999n`) |
| `WithMmapMemory` | n/a | **on** (linux/darwin, amd64/arm64) | lazily zeroed linear memory, no copy on grow, freed on `Close` |
| `WithMemoryCapacityFromMax` | n/a | off | see measurements below |
| clocks / entropy | fake walltime, deterministic rand | system walltime + nanotime, `crypto/rand` | |

### Measurements

Measured on linux/amd64 (48-core Railway container, Go 1.25, wazero 1.11);
CI re-runs both on `ubuntu-24.04` (`TestFreshInstanceLatency`,
`TestPeakRSSAtOOM`).

**Fresh instance** — `Runtime.Instance` + `Eval` + `Close` with a deadline on
the context, 3000 runs: **p50 0.18 ms**, p90 0.50 ms, p99 1.05 ms locally;
**p50 0.26 ms**, p90 0.41 ms, p99 0.64 ms on the GitHub `ubuntu-24.04`
(linux/amd64) runner. CI fails if p50 exceeds 0.5 ms there. Upstream's embedded wasm took 0.69 ms for create
+ close alone; the gains come from coalescing Wizer's ~3.8k data segments into
2 (`tools/wasm-merge-data`) and from mmap-backed memory (no 1.25 MiB zeroing
per instance).

**Peak RSS at OOM** — page cap 128 MiB, JS heap limit 1 GiB (so the page cap
is hit first), script allocating until `memory_limit_exceeded`:

| linear memory | instances | peak RSS − baseline | instance create + eval + close |
|---|---|---|---|
| mmap (default) | 1 | 127 MiB | 0.14 ms |
| mmap (default) | 8 | 972 MiB | |
| Go heap | 1 | 384 MiB | 0.34 ms |
| Go heap | 8 | 2169 MiB | |
| Go heap + `WithMemoryCapacityFromMax` | 1 | 115 MiB | **6.8 ms** |
| Go heap + `WithMemoryCapacityFromMax` | 8 | 1013 MiB | |

(GitHub runners, 1 instance: mmap 127 MiB; heap 343 MiB on linux/amd64 and
377 MiB on darwin/arm64; heap + capacity-from-max 110/128 MiB.)

With the Go-heap allocator every `memory.grow` copies and the old buffers wait
for the GC, so the peak is ~3× the cap. `WithMemoryCapacityFromMax` removes
that but zeroes the whole cap on every instantiation (6.8 ms at 128 MiB), so
it stays **off** by default; the mmap allocator gets the low peak without the
creation cost and is the default where available.

**Cost of close-on-context-done** — wazero instruments every loop back-edge:
a 100k-iteration JS loop takes ~60 ms instead of ~11–16 ms, and a trivial
`Eval` ~13 µs instead of ~5 µs. Disable it with
`WithCloseOnContextDone(false)` when the cooperative interrupt (which also
observes the context and covers JS loops, regex backtracking and host calls)
is enough.

### Reproducible build

`scripts/build-wasm.sh` produces a byte-identical `runtime.wasm` from the same
commit:

- pinned tools, downloaded with SHA-256 verification by
  `scripts/toolchain.sh`: Rust 1.99.0 (`kafig-runtime/rust-toolchain.toml`),
  wasi-sdk 34, binaryen 133, Wizer 11.0.3; crates via `Cargo.lock --locked`;
  QuickJS-ng sources vendored in `kafig-runtime/vendor/rquickjs-sys`;
- host paths remapped (`--remap-path-prefix`, `-ffile-prefix-map`);
- the Wizer snapshot runs against fixed WASI/env stubs
  (`kafig-runtime/wizer-*-stub.wat`) instead of host WASI, so no build-time
  clock or randomness leaks into the snapshot;
- `tools/wasm-merge-data` is deterministic.

CI builds the wasm twice in independent jobs (different checkout paths,
`CARGO_HOME` and toolchain directories) and requires both SHA-256s to equal
the committed `runtime.wasm.sha256`. The canonical build host is linux/x86_64.

### Update policy

- **Source only.** The sandbox executes JavaScript source; bytecode from
  users is never accepted (enforced by sealed bytecode, see above).
- **rquickjs / QuickJS-ng CVEs.** Watch QuickJS-ng and rquickjs security
  advisories. To pick up a fix: bump `RQUICKJS_SYS_VERSION`/`RQUICKJS_SYS_SHA256`
  in `scripts/vendor-rquickjs-sys.sh` and `rquickjs` in
  `kafig-runtime/Cargo.toml` to the matching release, run
  `scripts/vendor-rquickjs-sys.sh` (refresh any patch in
  `kafig-runtime/patches/rquickjs-sys` that no longer applies), then
  `scripts/build-wasm.sh --install`, run the test suite (the global-object
  allowlist test flags new built-ins for review) and commit `runtime.wasm`
  with its `.sha256` in the same PR. A fix that cannot wait for an rquickjs
  release goes in as a patch file in `patches/rquickjs-sys`.
- **Toolchain bumps** (Rust, wasi-sdk, binaryen, Wizer) change the checksum:
  update `rust-toolchain.toml` / `scripts/toolchain.sh` (version + archive
  SHA-256) and commit the rebuilt wasm in the same PR.
- **Release.** Merge to `main`, then tag both `vX.Y.Z` (GitHub release with
  the verified `runtime.wasm`, see `.github/workflows/release.yml`) and
  `kafig-go/vX.Y.Z` (the Go module version) on the same commit. Patch
  releases for security fixes; consumers update with
  `go get github.com/iota-uz/kafig/kafig-go@vX.Y.Z`.

## When Kafig Might Not Be Right

- **High-throughput number crunching.** QuickJS is an interpreter. If your workload is CPU-bound computation (matrix math, image processing, cryptography), you'll see 10-100x slower execution compared to V8's JIT. Consider running those workloads natively.
- **Full Node.js compatibility.** There's no `require`, no `fs`, no `Buffer`, no Node.js standard library. If your scripts depend on the Node.js ecosystem, kafig won't work without significant adaptation.
- **ES modules.** If you need `import`/`export` syntax or dynamic `import()`, kafig doesn't support it. You can use a bundler like [esbuild](https://esbuild.github.io/) to get around this.
- **Large JSON payloads.** Every RPC call serializes parameters and results as JSON across the WASM boundary. If you're passing multi-megabyte blobs back and forth, the serialization overhead may matter.

## License

MIT. Copyright (c) 2026 Merlin (original kafig); fork changes by iota-uz under
the same license. QuickJS-ng and rquickjs are MIT-licensed (see
`kafig-runtime/vendor/rquickjs-sys`).
