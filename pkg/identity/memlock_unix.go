//go:build linux || darwin

package identity

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// mlockBytes best-effort mlock(2)'s the memory backing b so it cannot be
// paged to swap. It is deliberately non-fatal: an unprivileged process
// without CAP_IPC_LOCK (Linux) or a low RLIMIT_MEMLOCK (both platforms,
// commonly 64KiB-8MiB by default) will fail here, and dontguess must still
// start — this is defense-in-depth (dontguess-973 C3), not a hard
// requirement. Callers log the outcome; they never treat failure as fatal.
func mlockBytes(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return unix.Mlock(b)
}

// scalarBytes returns an unsafe []byte view over the fixed-size in-memory
// representation pointed to by v (e.g. the address of btcec's ModNScalar
// embedded by value inside *btcec.PrivateKey). Go's garbage collector does
// not move heap-allocated objects at runtime (non-compacting), so the
// address is stable for the object's lifetime once it exists on the heap —
// mlock'ing it here is safe against the GC relocating the page out from
// under the lock.
//
// This does NOT protect copies: Serialize()/PrivHex() and any value-copy of
// the scalar produce unlocked, unzeroed copies elsewhere on the heap or
// stack. Locking the ORIGINAL scalar in the loaded Secp256k1Identity is the
// best-effort property this delivers, not a guarantee that no copy of the
// key material ever transits unlocked memory (PrivHex() for on-disk
// persistence is one such necessary, short-lived exception — see the
// custody threat model, docs/design/content-confidentiality-envelope-541.md
// §4.2/§3.5).
func scalarBytes(v unsafe.Pointer, size uintptr) []byte {
	return unsafe.Slice((*byte)(v), size)
}
