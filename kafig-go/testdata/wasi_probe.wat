;; WASI capability probe for isolation_test.go. Instantiated with the exact
;; ModuleConfig that kafig uses for runtime.wasm instances, it reports what a
;; guest could reach through WASI. Rebuild the .wasm with binaryen:
;;   wasm-as testdata/wasi_probe.wat -o testdata/wasi_probe.wasm
(module
  (import "wasi_snapshot_preview1" "fd_prestat_get" (func $fd_prestat_get (param i32 i32) (result i32)))
  (import "wasi_snapshot_preview1" "environ_sizes_get" (func $environ_sizes_get (param i32 i32) (result i32)))
  (import "wasi_snapshot_preview1" "args_sizes_get" (func $args_sizes_get (param i32 i32) (result i32)))
  (import "wasi_snapshot_preview1" "fd_write" (func $fd_write (param i32 i32 i32 i32) (result i32)))
  (import "wasi_snapshot_preview1" "path_open" (func $path_open (param i32 i32 i32 i32 i32 i64 i64 i32 i32) (result i32)))
  (memory (export "memory") 1)
  ;; "x\n" at 64, iovec {ptr=64,len=2} at 0, "etc/hosts" at 128
  (data (i32.const 64) "x\n")
  (data (i32.const 128) "etc/hosts")

  ;; errno of fd_prestat_get(fd): 8 (EBADF) means "no preopened directory".
  (func (export "prestat") (param $fd i32) (result i32)
    (call $fd_prestat_get (local.get $fd) (i32.const 256)))

  ;; number of environment variables visible to the guest
  (func (export "env_count") (result i32)
    (drop (call $environ_sizes_get (i32.const 256) (i32.const 260)))
    (i32.load (i32.const 256)))

  ;; number of command-line arguments visible to the guest
  (func (export "args_count") (result i32)
    (drop (call $args_sizes_get (i32.const 256) (i32.const 260)))
    (i32.load (i32.const 256)))

  ;; writes "x\n" to fd; returns errno
  (func (export "write_fd") (param $fd i32) (result i32)
    (i32.store (i32.const 0) (i32.const 64))
    (i32.store (i32.const 4) (i32.const 2))
    (call $fd_write (local.get $fd) (i32.const 0) (i32.const 1) (i32.const 300)))

  ;; path_open(dirfd, "etc/hosts"); returns errno (non-zero = denied)
  (func (export "open_hosts") (param $dirfd i32) (result i32)
    (call $path_open (local.get $dirfd) (i32.const 0) (i32.const 128) (i32.const 9)
      (i32.const 0) (i64.const 2) (i64.const 0) (i32.const 0) (i32.const 400)))
)
