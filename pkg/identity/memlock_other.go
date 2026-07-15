//go:build !linux && !darwin

package identity

import "unsafe"

// mlockBytes is a no-op stub on platforms without a wired mlock(2)
// equivalent here (e.g. Windows). Best-effort per dontguess-973 C3: dontguess
// must still run without this hardening, so callers never treat this as
// fatal.
func mlockBytes(b []byte) error {
	return nil
}

// scalarBytes mirrors the unix build's helper so callers compile identically;
// on this build it is unused by mlockBytes but kept so call sites need no
// build tags of their own.
func scalarBytes(v unsafe.Pointer, size uintptr) []byte {
	return unsafe.Slice((*byte)(v), size)
}
