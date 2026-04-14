package main

// IPC op constants for the operator unix domain socket protocol.
// All string literals in serve.go and operator.go must use these constants —
// no bare string literals for op names (dontguess-0b1).
const (
	OpListHeld  = "list-held"
	OpAcceptPut = "accept-put"
	OpRejectPut = "reject-put"
)
