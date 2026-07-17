package main

// IPC op constants for the operator unix domain socket protocol.
// All string literals in serve.go and operator.go must use these constants —
// no bare string literals for op names (dontguess-0b1).
const (
	OpListHeld  = "list-held"
	OpAcceptPut = "accept-put"
	OpRejectPut = "reject-put"
	// OpMetrics returns the running engine's degradation counters
	// (exchange.DegradationCounts) — dispatch trust-gate rejections, counted
	// and alarmed rather than silently dropped (docs/design/relay-transport.md
	// §2.4a D4 + §3, dontguess-388). Consumed by `dontguess status`.
	OpMetrics = "metrics"
	// OpMint is the operator genesis-funding god-button (design §4): mint scrip
	// to an agent so the first team-tier buy does not deadlock on
	// ErrBudgetExceeded. Operator-only (the socket lives in a 0700 dir inside the
	// trust boundary), audit-logged. Consumed by `dontguess mint`.
	OpMint = "mint"
	// OpPut is the individual-tier (zero-relay) put op (design §3.3, item
	// ed2-E, dontguess-2b4): the client routes a put through the already-running
	// `serve` over this socket instead of appending to events.jsonl directly
	// (single-writer invariant, relay-transport.md §0). The engine ingests the
	// record via exchange.Engine.IngestLocalRecord (localMu-guarded fold), then
	// auto-accepts it into matchable inventory. ScripStore==nil-only; no mint
	// path, no scrip movement. Consumed by `dontguess put` when
	// DONTGUESS_RELAY_URLS is unset.
	OpPut = "put"
	// OpBuy is the individual-tier (zero-relay) buy op (design §3.3, item
	// ed2-E, dontguess-2b4): the client routes a buy through the already-running
	// `serve` over this socket. The engine ingests+dispatches the buy
	// (IngestLocalRecord), then the handler blocks SERVER-SIDE up to a bounded
	// window (a dedicated deadline > operatorConnDeadline) for the e-tagged
	// match, and returns match + inline content over the socket — no relay, no
	// settle chain, no scrip. Consumed by `dontguess buy` when
	// DONTGUESS_RELAY_URLS is unset.
	OpBuy = "buy"
	// OpAllowlist is the live fleet-allowlist hot-reload op (dontguess-113, design
	// §3 + §9 Gate B/P6): `dontguess allowlist add|remove` on a RUNNING operator
	// mutates the live TrustChecker KeySet + republishes the operator-signed
	// kind-30078 roster + persists Config.FleetAllowlist, all sub-second with NO
	// restart (so admitting a member never re-triggers the 61a Since=0 history
	// re-read). Like OpMint it carries an operator-key-signed authorization
	// (allowlist_auth) verified server-side (verifyAllowlistAuth): reaching the
	// 0700 socket is necessary but NOT sufficient (ADV-16). Consumed by
	// `dontguess allowlist add|remove` when the operator is running.
	OpAllowlist = "allowlist"
	// OpListAssigns is the assign-discovery IPC op (item dontguess-d26, #2 AGENT
	// DOOR): reads the running engine's live State directly for every open/
	// claimable AssignRecord (State.AllActiveAssigns, filtered to
	// ExclusiveSender=="" or ==the caller's key). Unlike OpMint/OpAllowlist this
	// is a plain READ with no signed authorization — the assign lifecycle it
	// exposes is already publicly broadcast (an operator-authored exchange:assign
	// is not confidential), so socket reachability alone is a sufficient trust
	// boundary here, matching OpMetrics/OpListHeld. Consumed by `dontguess
	// assigns` on the individual (zero-relay) tier; a relay-attached tier
	// discovers the same information by subscribing the relay directly
	// (pkg/relayclient.FetchOpenAssigns) since operator-authored assign messages
	// are published there too (pkg/relay/outbox.go).
	//
	// `dontguess assign claim`/`assign complete` are NOT wired to an individual-
	// tier IPC op: OpPut/OpBuy's "zero identity ceremony" design mints a FRESH
	// random sender key on every call, but applyAssignComplete requires
	// msg.Sender == the claim's ClaimantKey (state_assign.go) — a binding that
	// cannot survive across two separate CLI invocations with no persisted
	// identity between them. Claim/complete require a relay-attached (team) tier
	// where the agent signs consistently from its walk-up .dg/ identity.
	OpListAssigns = "list-assigns"
)
