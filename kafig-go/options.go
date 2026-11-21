package kafig

import (
	"io"
	"log/slog"

	"github.com/tetratelabs/wazero"
)

// runtimeOptions holds configuration for a Runtime and its Instances.
type runtimeOptions struct {
	module                []byte // custom WASM binary; nil = use embedded default
	prelude               string
	preludeBytecode       []byte // compiled by New(), used by each Instance
	logger                *slog.Logger
	closeOnContextDone    bool
	compilationCache      wazero.CompilationCache
	wasmMemoryLimitPages  uint32 // 0 = unlimited (1 page = 64KB)
	memoryCapacityFromMax bool
	mmapMemory            bool      // back linear memory with mmap (see WithMmapMemory)
	randSource            io.Reader // WASI random_get source; default crypto/rand.Reader
}

// RuntimeOption configures a Runtime created with New.
type RuntimeOption func(*runtimeOptions)

// WithModule sets a custom WASM binary to use instead of the embedded default.
// The binary must export the same functions as the kafig-runtime module (alloc,
// dealloc, eval, eval_compiled, dispatch_event, etc.) and import the "env"
// host functions (host_rpc, host_set_result, host_rpc_sync, host_should_interrupt,
// host_promise_rejection).
func WithModule(data []byte) RuntimeOption {
	return func(o *runtimeOptions) { o.module = data }
}

// WithPrelude sets a JavaScript source string that is evaluated synchronously
// on every new Instance before any user Eval calls. Use this to define global
// helper functions or constants that user scripts can rely on.
func WithPrelude(source string) RuntimeOption {
	return func(o *runtimeOptions) { o.prelude = source }
}

// WithLogger sets a structured logger for internal debug logging. When set,
// the runtime logs instance creation, RPC dispatch, and errors.
func WithLogger(logger *slog.Logger) RuntimeOption {
	return func(o *runtimeOptions) { o.logger = logger }
}

// WithCloseOnContextDone controls wazero's context-done checking. When
// enabled (the default), in-flight WASM function calls are terminated if the
// context passed to Eval, EvalCompiled, or DispatchEvent is cancelled or times
// out; the Instance is closed in that case and must be discarded. This is the
// hard stop for untrusted code: the cooperative interrupt (which also observes
// the context) cannot fire inside long-running native QuickJS operations such
// as huge BigInt arithmetic. Disabling it saves a small per-call overhead but
// lets such code block the goroutine past the deadline.
func WithCloseOnContextDone(closeOnContextDone bool) RuntimeOption {
	return func(o *runtimeOptions) { o.closeOnContextDone = closeOnContextDone }
}

// WithCompilationCache provides a custom wazero compilation cache. When set,
// the Runtime uses this cache instead of creating its own. This allows sharing
// the compiled WASM module across multiple Runtime instances, avoiding
// redundant compilation. The caller is responsible for closing the cache.
func WithCompilationCache(cache wazero.CompilationCache) RuntimeOption {
	return func(o *runtimeOptions) { o.compilationCache = cache }
}

// WithWASMMemoryLimitPages sets the maximum WASM linear memory in pages
// (1 page = 64KB). This limits the total memory available to each WASM
// module instance including the QuickJS heap, stack, and Rust allocator
// overhead. Default: 0 (unlimited).
func WithWASMMemoryLimitPages(pages uint32) RuntimeOption {
	return func(o *runtimeOptions) { o.wasmMemoryLimitPages = pages }
}

// WithMemoryCapacityFromMax makes the Go-heap linear memory allocate its
// maximum size up front (wazero RuntimeConfig.WithMemoryCapacityFromMax), so
// memory.grow never copies. It has an effect only when the mmap allocator is
// off (see WithMmapMemory) and a page limit is set. Default: false; see the
// README for the peak-RSS measurements behind that choice.
func WithMemoryCapacityFromMax(enabled bool) RuntimeOption {
	return func(o *runtimeOptions) { o.memoryCapacityFromMax = enabled }
}

// WithMmapMemory controls whether each Instance's WASM linear memory is backed
// by an anonymous mmap reservation instead of a Go-heap slice. Untouched pages
// then cost neither time nor RSS, growth never copies, and Close returns the
// memory to the OS immediately. Default: true on linux and darwin (amd64,
// arm64); ignored on other platforms.
func WithMmapMemory(enabled bool) RuntimeOption {
	return func(o *runtimeOptions) { o.mmapMemory = enabled }
}

// WithRandSource sets the entropy source behind WASI random_get, which seeds
// Math.random in each Instance. Default: crypto/rand.Reader, so every Instance
// gets an independent sequence. Pass a deterministic reader only for
// reproducible tests.
func WithRandSource(r io.Reader) RuntimeOption {
	return func(o *runtimeOptions) { o.randSource = r }
}

// instanceOptions holds configuration for a single Instance.
type instanceOptions struct {
	router                  *RPCRouter
	logger                  *slog.Logger
	preludeBytecode         []byte
	interruptCallback       func(opcodes uint64, cpuTimeUs uint64) bool
	promiseRejectionHandler func(*JsError) bool
	jsMemoryLimit           uint32 // 0 = use default (32MB)
	bytecodeKey             []byte // set by Runtime.Instance
}

// InstanceOption configures a Instance created with Instance.
type InstanceOption func(*instanceOptions)

// WithRouter sets an RPCRouter for the Instance. Sync RPC calls
// (host.rpcSync in JS) are dispatched to handlers registered via
// RPCRouter.WithSync, and async RPC calls (host.rpc in JS) go to
// RPCRouter.WithAsync handlers (with fallback to sync handlers).
func WithRouter(router *RPCRouter) InstanceOption {
	return func(o *instanceOptions) { o.router = router }
}

// WithInterruptCallback sets a callback invoked every ~10,000 branch/loop
// opcodes with the current opcode count and CPU time in microseconds. Return
// true to interrupt execution.
func WithInterruptCallback(fn func(opcodes uint64, cpuTimeUs uint64) bool) InstanceOption {
	return func(o *instanceOptions) { o.interruptCallback = fn }
}

// WithPromiseRejectionHandler sets a callback invoked whenever an unhandled
// promise rejection occurs. The callback receives the JsError describing the
// rejection and returns true to interrupt execution or false to continue.
func WithPromiseRejectionHandler(fn func(*JsError) bool) InstanceOption {
	return func(o *instanceOptions) { o.promiseRejectionHandler = fn }
}

// WithJSMemoryLimit sets the QuickJS heap memory limit in bytes.
// Default: 32MB (set in the WASM runtime). Set to 0 to use the default.
func WithJSMemoryLimit(bytes uint32) InstanceOption {
	return func(o *instanceOptions) { o.jsMemoryLimit = bytes }
}

// evalOptions holds per-call configuration for Eval, EvalCompiled, and
// DispatchEvent.
type evalOptions struct {
	async_ bool
}

// EvalOption configures a single Eval, EvalCompiled, or DispatchEvent call.
type EvalOption func(*evalOptions)

// WithAsync enables async evaluation. For Eval, this sets JS_EVAL_FLAG_ASYNC so
// that top-level await is supported and the last expression result is returned
// as a resolved Promise value. For Compile, it bakes the async flag into the
// bytecode. For all call types, it drains the QuickJS job queue and services
// RPC calls after execution.
func WithAsync() EvalOption {
	return func(o *evalOptions) { o.async_ = true }
}
