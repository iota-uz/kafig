use rquickjs::qjs;

use crate::error::{extract_exception, send_exception};
use crate::tracking::USED_RPC_CALLS;

/// Runs every pending job (promise reactions, async continuations).
///
/// A job only fails with an uncatchable error: an interrupt (CPU limit,
/// `Interrupt()`, a done context on the host) or out-of-memory. Such a failure
/// is reported to the host as an error instead of silently ending the drain —
/// otherwise `for (;;) await null` stopped by an interrupt would look like an
/// empty success. Remaining jobs are left queued and flagged as stale so the
/// next invocation discards them.
pub(crate) fn drain_jobs(ctx: &rquickjs::Ctx<'_>) {
    unsafe {
        let rt = qjs::JS_GetRuntime(ctx.as_raw().as_ptr());
        loop {
            let mut job_ctx: *mut qjs::JSContext = std::ptr::null_mut();
            let r = qjs::JS_ExecutePendingJob(rt, &mut job_ctx);
            if r == 0 {
                return;
            }
            if r < 0 {
                // Single-context runtime: the failing job ran in `ctx`.
                // send_exception reports "interrupted" when the interrupt
                // handler caused this (see error::interrupted).
                let (msg, name, stack) = extract_exception(ctx);
                send_exception(&name, &msg, stack.as_deref());
                USED_RPC_CALLS = true;
                return;
            }
        }
    }
}
