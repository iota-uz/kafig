;; Deterministic WASI stand-in used ONLY while Wizer snapshots the runtime
;; (scripts/build-wasm.sh). The snapshot must not depend on the build host's
;; clock, randomness, environment or stdio, otherwise two builds of the same
;; commit would produce different runtime.wasm checksums.
;;
;; A preloaded module cannot access the guest's linear memory, so clock and
;; random calls report success without writing: the guest reads back whatever
;; its (deterministic) buffer already held. Everything else traps, which makes
;; Wizer fail loudly if initialization starts depending on more of WASI.
;; At run time the real WASI implementation from wazero is used instead.
(module
  (func (export "random_get") (param i32 i32) (result i32) (i32.const 0))
  (func (export "clock_time_get") (param i32 i64 i32) (result i32) (i32.const 0))
  (func (export "environ_get") (param i32 i32) (result i32) unreachable)
  (func (export "environ_sizes_get") (param i32 i32) (result i32) unreachable)
  (func (export "fd_close") (param i32) (result i32) unreachable)
  (func (export "fd_fdstat_get") (param i32 i32) (result i32) unreachable)
  (func (export "fd_seek") (param i32 i64 i32 i32) (result i32) unreachable)
  (func (export "fd_write") (param i32 i32 i32 i32) (result i32) unreachable)
  (func (export "proc_exit") (param i32) unreachable)
)
