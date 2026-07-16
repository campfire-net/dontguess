package main

// serve_redeem.go — the OPERATOR-SIDE half of self-service onboarding (design
// docs/design/onboarding-tiered-scaling-federation.md §1 + §9 Gate B/P8, ADV-15).
//
// The relay reader (serve_relay.go runReader) routes a received kind-3410 REDEEM
// event to redeemHandler.handle. This is where the operator does 100% of the
// authoritative verification and promotion — trusting NOTHING about whether a relay
// gated the write (ADV-2, the same rule the roster fold and VerifyOperatorAuthorship
// obey). A dumb/open relay that forwards a forged, expired, or replayed redeem
// changes the fleet by EXACTLY NOTHING.
//
// On a VALID redeem it, in order:
//
//	1. rejects a REPLAY — the grant id is checked against a DURABLE redeemed-id set
//	   (redeemedStore, persisted to disk) so a replay is rejected even AFTER an
//	   operator restart;
//	2. PERSISTS the grant id as redeemed BEFORE any promotion or mint — so a crash
//	   between here and the mint can only UNDER-grant (member admitted-but-unfunded,
//	   fixable), never DOUBLE-mint (the genesis grant is never minted twice);
//	3. PROMOTES the member into BOTH gates live via the SAME operator-signed
//	   OpAllowlist path `dontguess allowlist add` uses (allowlistController.apply →
//	   live KeySet + operator-signed roster republish + config persist, §3); and
//	4. MINTS the optional genesis grant to the member (eng.MintScrip).
//
// Absorbs agent-init + allowlist add + mint into one redeem, exactly as the design
// promises. Authorization for the promotion is an operator-key signature the handler
// mints with the operator signer — the same gate `allowlist add` proves (ADV-16),
// so the redeem path cannot admit anyone the operator key could not.

import (
	"fmt"
	"sync"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
)

// redeemHandler owns the operator-side kind-3410 redeem verification + promotion.
// It is constructed once per serve process (team tier only) and shared by every
// relay leg's reader, so handle() is mutex-serialized: two legs may deliver echoes
// of the same redeem concurrently, and the one-time redemption check + persist must
// be atomic to prevent a double-admit/double-mint race.
type redeemHandler struct {
	operatorKey string               // pinned operator pubkey (hex) — the invite PIN
	ctrl        *allowlistController // live promotion path (KeySet + roster + config)
	eng         *exchange.Engine     // genesis grant mint
	redeemed    *redeemedStore       // durable one-time redeemed-grant-id set
	logf        func(format string, args ...any)
	nowUnix     func() int64

	mu sync.Mutex
}

// newRedeemHandler builds the handler over the live promotion controller, engine,
// and a durable redeemed-id store at storePath. A nil logf defaults to a no-op. It
// returns an error only if the redeemed-id store cannot be opened (a fail-closed
// refusal to serve redemptions with an unknown one-time set rather than silently
// re-admitting replays).
func newRedeemHandler(operatorKey string, ctrl *allowlistController, eng *exchange.Engine, storePath string, logf func(format string, args ...any)) (*redeemHandler, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	rs, err := openRedeemedStore(storePath)
	if err != nil {
		return nil, fmt.Errorf("redeemed-invite store: %w", err)
	}
	return &redeemHandler{
		operatorKey: operatorKey,
		ctrl:        ctrl,
		eng:         eng,
		redeemed:    rs,
		logf:        logf,
		nowUnix:     func() int64 { return time.Now().Unix() },
	}, nil
}

// handle applies one received kind-3410 redeem event. Every rejection is logged
// LOUD (LOCKED-5): a forged/expired/replayed redeem reaching the operator is
// security-relevant and never a silent drop. It is idempotent on a replay (the
// durable redeemed-id set) and never promotes/mints on any verification failure.
func (rh *redeemHandler) handle(ev *nostr.Event) {
	if rh == nil || ev == nil {
		return
	}

	// AUTHORITATIVE VERIFY (ADV-2): member signature, embedded operator-signed
	// invite, operator-key PIN, invite-id binding, and freshness — all re-derived
	// from the signed events themselves, never from any relay write policy.
	redeem, err := nostr.VerifyRedeem(ev, rh.operatorKey, rh.nowUnix())
	if err != nil {
		rh.logf("SECURITY: invite redeem REJECTED event %s: %v", shortRedeemID(ev.ID), err)
		return
	}
	member := redeem.MemberHexKey
	grant := redeem.Invite.GrantID

	rh.mu.Lock()
	defer rh.mu.Unlock()

	// (1) REPLAY: a grant id already redeemed is rejected — even across a restart,
	// because the redeemed set is durable. This is what makes the token single-use
	// and NOT a reusable bearer credential.
	if rh.redeemed.has(grant) {
		rh.logf("invite redeem: grant %s already redeemed — rejecting replay (member %s)", shortRedeemID(grant), shortHex(member))
		return
	}

	// (2) PERSIST redeemed FIRST (durable) so a crash before the mint below can only
	// under-grant, never double-mint. A persist failure fails closed: nothing is
	// promoted or minted.
	if err := rh.redeemed.add(grant); err != nil {
		rh.logf("SECURITY: invite redeem: persist redeemed grant %s FAILED: %v — NOT promoting (a promote+mint without a durable redeemed marker would re-admit/double-mint on the replay after restart)", shortRedeemID(grant), err)
		return
	}

	// (3) PROMOTE into both gates via the SAME operator-signed OpAllowlist path
	// `dontguess allowlist add` uses. Authorize with an operator-key signature the
	// handler mints from the operator signer — the promotion gate (ADV-16) is
	// satisfied by proof of the operator key, exactly as a manual admit is.
	if err := rh.promote(member); err != nil {
		rh.logf("invite redeem: grant %s redeemed but PROMOTE failed for member %s: %v (grant consumed; operator can `allowlist add` manually)", shortRedeemID(grant), shortHex(member), err)
		// Bail: an unadmitted member must NOT be funded. The redeemed marker is
		// already durable, so this grant is consumed — the operator can admit the
		// member manually (`allowlist add`) without re-running the mint.
		return
	}

	// (4) MINT the optional genesis grant. 0 = none.
	if redeem.Invite.GenesisScrip > 0 {
		if err := rh.eng.MintScrip(member, redeem.Invite.GenesisScrip); err != nil {
			rh.logf("invite redeem: grant %s admitted member %s but genesis mint of %d failed: %v", shortRedeemID(grant), shortHex(member), redeem.Invite.GenesisScrip, err)
			return
		}
	}
	rh.logf("invite redeem: grant %s ONBOARDED member %s (genesis %d scrip) — admitted to fleet KeySet + roster",
		shortRedeemID(grant), shortHex(member), redeem.Invite.GenesisScrip)
}

// promote admits memberHex into the live fleet via allowlistController.apply,
// authorized by a freshly-signed operator OpAllowlist auth event. Caller holds
// rh.mu. A nil controller (should not happen on the team tier) is a hard error.
func (rh *redeemHandler) promote(memberHex string) error {
	if rh.ctrl == nil {
		return fmt.Errorf("no allowlist controller wired")
	}
	if rh.ctrl.operatorSigner == nil {
		return fmt.Errorf("no operator signer to authorize promotion")
	}
	auth := buildAllowlistAuthEvent(allowlistActionAdd, memberHex, rh.nowUnix())
	if err := identity.SignEvent(rh.ctrl.operatorSigner, auth); err != nil {
		return fmt.Errorf("sign promotion auth: %w", err)
	}
	return rh.ctrl.apply(allowlistActionAdd, memberHex, auth)
}

// shortRedeemID abbreviates an id for diagnostics without dumping the full value.
func shortRedeemID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:8] + "…" + id[len(id)-4:]
}
