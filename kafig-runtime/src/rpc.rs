use rquickjs::{Error, Function};
use std::time::Instant;

use crate::error::{extract_exception, send_exception, send_runtime_error};
use crate::init::RUNTIME;
use crate::tracking::{sample_elapsed_us, EXEC_START};

#[unsafe(export_name = "resolve_rpc")]
pub extern "C" fn resolve_rpc(promise_id: i32, result_ptr: *const u8, result_len: usize) {
    call_js_rpc_settlement("__resolve_rpc", promise_id, result_ptr, result_len);
}

#[unsafe(export_name = "reject_rpc")]
pub extern "C" fn reject_rpc(promise_id: i32, error_ptr: *const u8, error_len: usize) {
    call_js_rpc_settlement("__reject_rpc", promise_id, error_ptr, error_len);
}

fn call_js_rpc_settlement(fn_name: &str, id: i32, ptr: *const u8, len: usize) {
    let js = match RUNTIME.get() {
        Some(r) => r,
        None => return,
    };

    let payload =
        unsafe { std::str::from_utf8(std::slice::from_raw_parts(ptr, len)).unwrap_or("null") };

    // Set EXEC_START so the interrupt handler tracks CPU time accurately
    // during promise settlement and any JS continuations that fire.
    unsafe {
        EXEC_START = Some(Instant::now());
    }

    js.ctx
        .with(|ctx| -> rquickjs::Result<()> {
            let func: Function = ctx.globals().get(fn_name)?;
            match func.call::<_, ()>((id, payload)) {
                Ok(()) => crate::jobs::drain_jobs(&ctx),
                // Settling the promise ran user continuations synchronously
                // and they failed uncatchably (interrupt, out of memory):
                // report the real cause rather than a generic wrapper error.
                Err(Error::Exception) => {
                    let (msg, name, stack) = extract_exception(&ctx);
                    send_exception(&name, &msg, stack.as_deref());
                    unsafe { crate::tracking::USED_RPC_CALLS = true };
                }
                Err(e) => return Err(e),
            }
            Ok(())
        })
        .unwrap_or_else(|e| send_runtime_error("runtime_error", &format!("{fn_name} error: {e}")));

    unsafe {
        sample_elapsed_us();
        EXEC_START = None;
    }
}
