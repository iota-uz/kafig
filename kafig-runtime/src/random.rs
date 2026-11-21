// Per-instance Math.random.
//
// QuickJS seeds Math.random from the clock when the context is created. That
// happens inside the Wizer snapshot, so every instance of a given
// runtime.wasm would replay the same sequence. Math.random is therefore
// replaced by xorshift128+ seeded lazily, on first use in each instance, from
// WASI random_get (crypto/rand on the Go host by default).

#[link(wasm_import_module = "wasi_snapshot_preview1")]
unsafe extern "C" {
    #[link_name = "random_get"]
    fn wasi_random_get(buf: *mut u8, len: usize) -> u16;
}

static mut STATE: [u64; 2] = [0, 0];
static mut SEEDED: bool = false;

fn seed() {
    let mut buf = [0u8; 16];
    let errno = unsafe { wasi_random_get(buf.as_mut_ptr(), buf.len()) };
    let mut s0 = u64::from_le_bytes(buf[..8].try_into().unwrap());
    let mut s1 = u64::from_le_bytes(buf[8..].try_into().unwrap());
    if errno != 0 || (s0 == 0 && s1 == 0) {
        // No entropy available: never leave the all-zero state (xorshift
        // would then return 0 forever).
        s0 = 0x9E37_79B9_7F4A_7C15;
        s1 = 0xD1B5_4A32_D192_ED03;
    }
    unsafe {
        STATE = [s0, s1];
        SEEDED = true;
    }
}

/// Returns a uniformly distributed f64 in [0, 1).
pub(crate) fn next_f64() -> f64 {
    unsafe {
        if !SEEDED {
            seed();
        }
        let mut s1 = STATE[0];
        let s0 = STATE[1];
        STATE[0] = s0;
        s1 ^= s1 << 23;
        s1 ^= s1 >> 17;
        s1 ^= s0;
        s1 ^= s0 >> 26;
        STATE[1] = s1;
        let r = STATE[0].wrapping_add(STATE[1]);
        (r >> 11) as f64 * (1.0 / (1u64 << 53) as f64)
    }
}
