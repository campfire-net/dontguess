package main

// serve_relay_ed2c_allowlist_test.go — dontguess-980: proves the live gap
// pkg/exchange/trust.go's TrustChecker.Level() exposed — OperationBuy is
// TrustAnonymous (defaultOperationLevels, trust.go) so a minted-but-NOT-
// fleet-allowlisted buyer's `dontguess buy` MATCHES fine, but every buyer-side
// settle phase (buyer-accept, complete, ...) requires TrustAllowlisted
// (defaultSettlePhaseLevels, trust.go), so the buyer's settle(buyer-accept) is
// silently dropped pre-fold at the dispatch trust gate (engine_core.go
// dispatch: TrustChecker.Check fails -> logged + counted -> dispatch returns
// nil, no fold, no reject emitted). From the CLIENT's perspective this is
// indistinguishable from an operator/relay stall: the per-phase await times
// out -> SettleOutcomeAmbiguous.
//
// This test drives the REAL client (relayclient.Buy + relayclient.Settle)
// through a REAL engine with a REAL TrustChecker whose fleet allowlist
// contains the seller but deliberately OMITS the buyer, over the same
// ed2cRelayHub websocket bridge serve_relay_ed2c_test.go uses. It asserts:
//  1. The match succeeds (OperationBuy is anonymous-admitted).
//  2. Settle terminates SettleOutcomeAmbiguous (not a hang, not Settled) —
//     the buyer-accept never reaches the fold.
//  3. relayclient.WriteSettleOutcome's rendered AMBIGUOUS block mentions
//     buyer-allowlist status as a possible cause (the settle.go fix under
//     this item), so the client-side guidance actually tells the operator
//     what to do: `dontguess allowlist add <buyer-npub>`.
import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/relayclient"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// newEd2cFixtureSellerOnlyAllowlist mirrors newEd2cFixture but wires a real
// *exchange.TrustChecker whose fleet allowlist (KeySet) contains ONLY the
// seller — never the buyer. The buyer is minted (scrip funded) by the caller
// exactly as the other ed2c tests do; minting and allowlisting are
// independent axes (dontguess-980 is precisely that the doc failed to say
// both are required for a buyer).
func newEd2cFixtureSellerOnlyAllowlist(t *testing.T) *ed2cFixture {
	t.Helper()
	hushRelayLogs(t)
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()

	fleet := exchange.NewKeySet(seller.PubKeyHex())
	tc, err := exchange.NewTrustChecker(operator.PubKeyHex(), fleet)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	st := newWireIDStack(t, ctx, ls, operator, dir+"/events.jsonl.pubcursor", tc)
	t.Cleanup(func() { cancel(); st.stop() })

	putEv := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		knownPutPayload(ed2cPutDesc, ed2cContent, ed2cTokenCost))
	st.conn.inject(putEv)
	waitFor(t, 8*time.Second, "seller put auto-accepts into inventory", func() bool {
		return len(st.eng.State().Inventory()) == 1
	})

	hub := newEd2cRelayHub(t, st.conn)
	return &ed2cFixture{st: st, hub: hub, seller: seller, operator: operator, ls: ls}
}

// TestEd2C_RunBuy_MintedButNotAllowlistedBuyer_SettleGoesAmbiguous_GuidanceEnumeratesAllowlist
// is the dontguess-980 ground-source proof. It does NOT mock the trust gate,
// the engine, or the relay wire — it reproduces the exact silent-drop path
// that a minted-but-unallowlisted buyer hits in production, then checks the
// printed guidance names the real fix.
func TestEd2C_RunBuy_MintedButNotAllowlistedBuyer_SettleGoesAmbiguous_GuidanceEnumeratesAllowlist(t *testing.T) {
	fx := newEd2cFixtureSellerOnlyAllowlist(t)
	buyer := newBuyerAgent(t)
	// Minted (funded) — but never allowlisted. This is the exact gap: minting
	// alone is not sufficient for a team-tier buyer to complete a purchase.
	fx.st.mintBuyer(t, buyer)
	if got := fx.st.scrip.Balance(buyer.PubKeyHex()); got != wireIDBuyerMint {
		t.Fatalf("buyer balance before buy = %d, want minted %d", got, wireIDBuyerMint)
	}

	conn := newClientConn(t, fx.hub.wsURL(), buyer)
	defer conn.Close()

	// ONE shared ctx for the WHOLE buy->settle chain (design §3.5: "the whole
	// buy->settle chain runs in ONE invocation... bound lives entirely in the
	// client ctx" — mirrored by every RunE call site, which builds a single ctx
	// from --timeout and passes it to both Buy and Settle). This also matters
	// mechanically: NewConn's watchdogDialer (relayclient.go) installs its
	// force-close goroutine racing whatever ctx is live AT DIAL TIME (the
	// first Send/Recv on the connection, here inside Buy) and holds it for the
	// connection's whole lifetime — a Settle call reusing the same conn under a
	// DIFFERENT, shorter ctx would NOT actually bound the read; the dial-time
	// ctx would still govern when the socket is force-closed. Sizing this at 5s
	// keeps the genuine-timeout proof below tight instead of needlessly wide.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	buy, err := relayclientBuy(ctx, conn, buyer)
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	// (1) The buy itself is unaffected: OperationBuy is TrustAnonymous
	// (defaultOperationLevels, trust.go) — an unallowlisted buyer still matches.
	assertClientMatch(t, buy)

	res, err := relayclientSettle(ctx, conn, buyer, buy, false)
	if err != nil {
		t.Fatalf("Settle returned a hard error, want a terminal AMBIGUOUS outcome: %v", err)
	}
	if res == nil {
		t.Fatalf("Settle returned nil result")
	}
	// (2) buyer-accept requires TrustAllowlisted (defaultSettlePhaseLevels,
	// trust.go) — the dispatch trust gate silently drops it pre-fold
	// (engine_core.go dispatch: TrustChecker.Check fails -> return nil, no
	// fold, no settle(buyer-accept-reject) emitted either — this is NOT the
	// insufficient-scrip path, it never reaches the engine at all). The
	// client's per-phase await on #e:[buyer-accept] therefore times out.
	if res.Outcome != relayclient.SettleOutcomeAmbiguous {
		t.Fatalf("settle outcome = %s, want ambiguous-timeout (buyer-accept should be silently dropped at the trust gate)", res.Outcome)
	}
	// No scrip should have moved — the hold handler in the engine never ran.
	if got := fx.st.scrip.Balance(buyer.PubKeyHex()); got != wireIDBuyerMint {
		t.Fatalf("buyer balance after ambiguous settle = %d, want unchanged %d (no fold occurred)", got, wireIDBuyerMint)
	}

	// (3) settle.go's WriteSettleOutcome (dontguess-980 fix) must enumerate
	// buyer-allowlist status as a cause of an AMBIGUOUS outcome, mirroring
	// buy.go's ambiguousResult() enumerated-causes pattern, and must name the
	// actionable operator command.
	var out bytes.Buffer
	relayclient.WriteSettleOutcome(&out, buy.BuyID, res)
	printed := out.String()
	if !strings.Contains(printed, "AMBIGUOUS") {
		t.Fatalf("printed settle outcome missing AMBIGUOUS block:\n%s", printed)
	}
	if !strings.Contains(printed, "allowlist") {
		t.Fatalf("printed AMBIGUOUS guidance does not mention allowlisting — a minted-but-unallowlisted buyer gets no actionable hint:\n%s", printed)
	}
	if !strings.Contains(printed, "dontguess allowlist add") {
		t.Fatalf("printed AMBIGUOUS guidance does not name the actionable operator command `dontguess allowlist add`:\n%s", printed)
	}
}
