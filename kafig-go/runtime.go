package kafig

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"runtime"

	"github.com/iota-uz/kafig/kafig-go/internal/wasm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Runtime holds a shared wazero.Runtime so that WASI, host imports, and the
// compiled WASM module are created once and reused across all Instances. Each
// Instance gets its own anonymous module instantiation with isolated linear
// memory and JS heap.
type Runtime struct {
	wazeroRuntime wazero.Runtime
	compiled      wazero.CompiledModule
	module        []byte // WASM binary (for compileBytecode's temporary runtime)
	cache         wazero.CompilationCache
	ownCache      bool // true if we created the cache and should close it
	opts          runtimeOptions
	bytecodeKey   []byte // HMAC key sealing Compile output (see bytecode.go)
}

// New pre-compiles the embedded WASM module and creates a shared wazero
// runtime with WASI and host imports. The returned Runtime can create any
// number of Instances via [Runtime.Instance].
//
// If WithPrelude is set, the prelude source is compiled to QuickJS bytecode
// at this point. Each Instance then evaluates the bytecode (skipping parsing).
func New(ctx context.Context, options ...RuntimeOption) (*Runtime, error) {
	opts := runtimeOptions{
		logger:             slog.Default(),
		closeOnContextDone: true,
		mmapMemory:         true,
		randSource:         rand.Reader,
	}
	for _, o := range options {
		o(&opts)
	}
	if !mmapMemorySupported {
		opts.mmapMemory = false
	}

	// Resolve the WASM binary: use the custom module if provided, otherwise
	// fall back to the embedded default.
	module := opts.module
	if module == nil {
		module = wasm.RuntimeWasm
	}

	cache := opts.compilationCache
	ownCache := cache == nil
	if ownCache {
		cache = wazero.NewCompilationCache()
	}

	// Build the shared wazero runtime configuration.
	cfg := wazero.NewRuntimeConfig().
		WithCompilationCache(cache).
		WithCloseOnContextDone(opts.closeOnContextDone).
		WithMemoryCapacityFromMax(opts.memoryCapacityFromMax)
	if opts.wasmMemoryLimitPages > 0 {
		cfg = cfg.WithMemoryLimitPages(opts.wasmMemoryLimitPages)
	}

	r := wazero.NewRuntimeWithConfig(ctx, cfg)

	// Instantiate WASI once — shared across all guest modules.
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	// Register "env" host module once with context-dispatched functions.
	if _, err := r.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(sharedHostRPC).Export("host_rpc").
		NewFunctionBuilder().WithFunc(sharedHostSetResult).Export("host_set_result").
		NewFunctionBuilder().WithFunc(sharedHostRPCSync).Export("host_rpc_sync").
		NewFunctionBuilder().WithFunc(sharedHostShouldInterrupt).Export("host_should_interrupt").
		NewFunctionBuilder().WithFunc(sharedHostPromiseRejection).Export("host_promise_rejection").
		Instantiate(ctx); err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("kafig: register host module: %w", err)
	}

	// Compile the guest module once into the cache.
	compiled, err := r.CompileModule(ctx, module)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("kafig: compile module: %w", err)
	}

	bytecodeKey, err := newBytecodeKey()
	if err != nil {
		r.Close(ctx)
		return nil, err
	}

	rt := &Runtime{
		bytecodeKey:   bytecodeKey,
		wazeroRuntime: r,
		compiled:      compiled,
		module:        module,
		cache:         cache,
		ownCache:      ownCache,
		opts:          opts,
	}

	// If a prelude is configured, compile it to bytecode now so each
	// Instance can skip parsing.
	if opts.prelude != "" {
		bytecode, err := rt.compileBytecode(ctx, opts.prelude, false)
		if err != nil {
			r.Close(ctx)
			if ownCache {
				cache.Close(ctx)
			}
			return nil, fmt.Errorf("kafig: compile prelude: %w", err)
		}
		rt.opts.preludeBytecode = bytecode
	}

	return rt, nil
}

// Instance creates a new isolated WASM instance. Each instance has its own
// linear memory, QuickJS heap, and host function bindings, but shares the
// compiled module and host infrastructure with other instances.
func (r *Runtime) Instance(ctx context.Context, options ...InstanceOption) (*Instance, error) {
	opts := instanceOptions{
		logger:          r.opts.logger,
		preludeBytecode: r.opts.preludeBytecode,
		bytecodeKey:     r.bytecodeKey,
	}
	for _, o := range options {
		o(&opts)
	}

	if opts.router == nil {
		return nil, fmt.Errorf("kafig: RPCRouter is required (use WithRouter)")
	}

	var alloc *mmapAllocator
	if r.opts.mmapMemory {
		alloc = &mmapAllocator{}
		ctx = experimental.WithMemoryAllocator(ctx, alloc)
	}

	inst, err := newInstance(ctx, r.wazeroRuntime, r.compiled, r.moduleConfig(), opts)
	if alloc != nil && alloc.mem != nil {
		if err != nil {
			alloc.mem.release()
			return nil, err
		}
		inst.memory = alloc.mem
		// Safety net for Instances that are dropped without Close.
		runtime.AddCleanup(inst, (*mmapMemory).release, alloc.mem)
	}
	return inst, err
}

// moduleConfig is the WASI view every Instance gets: the real wall clock and
// monotonic clock (Date.now, performance.now) and fresh entropy (the
// Math.random seed). Nothing else: no preopened directories, environment
// variables or arguments, stdin is empty, and stdout/stderr are discarded.
func (r *Runtime) moduleConfig() wazero.ModuleConfig {
	return wazero.NewModuleConfig().
		WithName("").
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(r.opts.randSource).
		WithStdin(eofReader{}).
		WithStdout(io.Discard).
		WithStderr(io.Discard)
}

// eofReader is an always-empty stdin.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// Compile compiles JavaScript source to QuickJS bytecode. The bytecode can be
// passed to [Instance.EvalCompiled] to skip parsing on every call. It is
// sealed with a key private to this Runtime: it is accepted by all Instances
// of this Runtime and rejected everywhere else (see bytecode.go). Treat it as
// an opaque, process-local cache entry; do not persist it.
//
// Use WithAsync() if the source uses top-level await — this bakes
// JS_EVAL_FLAG_ASYNC into the bytecode so that EvalCompiled returns a Promise
// whose resolved value becomes the result.
func (r *Runtime) Compile(ctx context.Context, source string, options ...EvalOption) ([]byte, error) {
	var opts evalOptions
	for _, o := range options {
		o(&opts)
	}
	bytecode, err := r.compileBytecode(ctx, source, opts.async_)
	if err != nil {
		return nil, err
	}
	return sealBytecode(r.bytecodeKey, bytecode), nil
}

// Close releases the shared wazero runtime (closing all remaining instances)
// and the compilation cache if it was created by this Runtime.
func (r *Runtime) Close(ctx context.Context) error {
	err := r.wazeroRuntime.Close(ctx)
	if r.ownCache {
		if cerr := r.cache.Close(ctx); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// compileBytecode creates a temporary WASM instance, compiles the given source
// to QuickJS bytecode via the compile() export, and returns the bytecode bytes.
func (r *Runtime) compileBytecode(ctx context.Context, source string, isAsync bool) ([]byte, error) {
	// Create a temporary wazero runtime with the shared compilation cache.
	wzr := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCompilationCache(r.cache).
		WithCloseOnContextDone(r.opts.closeOnContextDone))
	defer wzr.Close(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, wzr)

	// Stub host imports — the WASM module declares them, but compile()
	// doesn't call host_rpc. host_set_result may be called on compile error
	// via send_error, but we detect errors from the packed return value.
	if _, err := wzr.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _, _, _, _, _ uint32) {}).Export("host_rpc").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _, _, _ uint32) {}).Export("host_set_result").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _, _, _, _ uint32) uint64 { return 0 }).Export("host_rpc_sync").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _, _ uint64) int32 { return 0 }).Export("host_should_interrupt").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _, _ uint32) int32 { return 0 }).Export("host_promise_rejection").
		Instantiate(ctx); err != nil {
		return nil, fmt.Errorf("register host module: %w", err)
	}

	compiled, err := wzr.CompileModule(ctx, r.module)
	if err != nil {
		return nil, fmt.Errorf("compile module: %w", err)
	}

	module, err := wzr.InstantiateModule(ctx, compiled, wazero.NewModuleConfig())
	if err != nil {
		return nil, fmt.Errorf("instantiate module: %w", err)
	}

	fnCompile := module.ExportedFunction("compile")
	fnAlloc := module.ExportedFunction("alloc")
	fnDealloc := module.ExportedFunction("dealloc")
	if fnCompile == nil || fnAlloc == nil || fnDealloc == nil {
		return nil, fmt.Errorf("WASM module missing required export (compile, alloc, or dealloc)")
	}

	// Allocate and write source into WASM memory.
	sourceBytes := []byte(source)
	allocRes, err := fnAlloc.Call(ctx, uint64(len(sourceBytes)))
	if err != nil {
		return nil, fmt.Errorf("alloc: %w", err)
	}
	ptr := uint32(allocRes[0])
	if ptr == 0 {
		return nil, fmt.Errorf("alloc returned null")
	}
	module.Memory().Write(ptr, sourceBytes)

	// Call compile(ptr, len, is_async) → packed u64
	isAsyncArg := uint64(0)
	if isAsync {
		isAsyncArg = 1
	}
	results, err := fnCompile.Call(ctx, uint64(ptr), uint64(len(sourceBytes)), isAsyncArg)
	if err != nil {
		return nil, fmt.Errorf("compile call: %w", err)
	}
	fnDealloc.Call(ctx, uint64(ptr), uint64(len(sourceBytes))) //nolint:errcheck

	packed := results[0]
	if packed == 0 {
		return nil, fmt.Errorf("compilation failed (syntax error or invalid source)")
	}

	// Unpack: high 32 bits = pointer, low 32 bits = length
	bcPtr := uint32(packed >> 32)
	bcLen := uint32(packed & 0xFFFFFFFF)

	// Read bytecode from WASM memory and copy to Go-owned slice.
	bcData, ok := module.Memory().Read(bcPtr, bcLen)
	if !ok {
		return nil, fmt.Errorf("failed to read bytecode from WASM memory")
	}
	bytecode := make([]byte, len(bcData))
	copy(bytecode, bcData)

	// Dealloc the WASM-side buffer.
	fnDealloc.Call(ctx, uint64(bcPtr), uint64(bcLen)) //nolint:errcheck

	return bytecode, nil
}
