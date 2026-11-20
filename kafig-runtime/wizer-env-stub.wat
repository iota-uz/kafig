;; Host imports are never called during Wizer initialization; these stubs only
;; satisfy instantiation and trap if that assumption is ever broken.
(module
  (func (export "host_rpc") (param i32 i32 i32 i32 i32) unreachable)
  (func (export "host_set_result") (param i32 i32 i32) unreachable)
  (func (export "host_rpc_sync") (param i32 i32 i32 i32) (result i64) unreachable)
  (func (export "host_should_interrupt") (param i64 i64) (result i32) unreachable)
  (func (export "host_promise_rejection") (param i32 i32) (result i32) unreachable)
)
