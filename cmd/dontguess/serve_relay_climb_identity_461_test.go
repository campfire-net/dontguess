package main

// serve_relay_climb_identity_461_test.go — the dontguess-461 E2E ground-source
// (design §6 ADV-17 identity migration + §9 Gate B/D climb): the solo→fleet CLIMB
// proves TWO invariants at once, against the REAL nostr operator identity, a REAL
// exchange engine fold, and the REAL relay Outbox egress fence:
//
//	(1) IDENTITY STABLE — the SAME persisted secp256k1 nostr operator key is
//	    State.OperatorKey both solo (before) and fleet (after) the climb. The climb
//	    NEVER re-mints (loadOrCreateNostrOperatorIdentity is idempotent), and
//	    historical solo operator records authored under a pre-P3 OPAQUE local key
//	    RE-ATTRIBUTE to that stable key via the wire-alias (RegisterWireAlias /
//	    applyLegacyOperatorAlias) — so a legacy operator put-accept still folds
//	    under State.OperatorKey instead of being dropped at the sender-must-be-
//	    operator gate (no Sender mismatch). A load-bearing NO-ALIAS control proves
//	    the re-attribution is what makes the historical record fold, not luck.
//
//	(2) NO PLAINTEXT REPUBLISH — the individual (solo) tier stores content as
//	    cleartext (§541 §6); on the climb the relay Outbox tails that SAME log. The
//	    climb egress fence (establishClimbWatermark → WithClimbFence) keeps every
//	    pre-climb plaintext record LOCAL-ONLY: ZERO reach the relay wire, and the
//	    secret plaintext appears in NO published event. A post-climb operator
//	    emission ABOVE the watermark DOES publish — signed by the SAME nostr key,
//	    tying the two invariants together on the wire — and a load-bearing NO-FENCE
//	    twin republishes the entire plaintext corpus, proving assertion (2) is a
//	    real fence, not a vacuous empty-log pass.
//
// Distinct from serve_relay_climb_fence_e18d_test.go (which proves the Outbox
// fence with a throwaway generated key and no identity dimension) and from
// pkg/exchange/climb_fence_grandfather_*_test.go (grandfathered-inventory match/
// deliver leg): THIS test drives the operator-IDENTITY continuity across the climb
// — the persisted nostr key, the wire-alias re-attribution of historical solo
// records, and that same key signing the post-climb fleet egress.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// putAcceptPayload461 is the operator's put-accept body (mirrors the 9d1 e2e
// fixture): a price and an empty expires_at (default TTL, stays live here).
func putAcceptPayload461(t *testing.T) []byte {
	t.Helper()
	p, err := json.Marshal(map[string]any{"price": int64(100), "expires_at": ""})
	if err != nil {
		t.Fatalf("marshal put-accept: %v", err)
	}
	return p
}

// foldSoloLog builds an engine with State.OperatorKey=operatorKey over a fresh
// copy of the mixed solo log (native nostr-key put + a legacy-key operator
// put-accept referencing it), optionally registering the legacy→nostr wire-alias
// BEFORE the startup fold — exactly as serve.go's applyLegacyOperatorAlias does
// before eng.Start. It folds synchronously (waits on OnStarted) and returns the
// live inventory count. registerAlias registers the alias; the no-alias control
// passes registerAlias=false to prove the re-attribution is load-bearing.
func foldSoloLog(t *testing.T, dir, operatorKey, sellerKey, legacyKey string, registerAlias bool) (opKeyAfterFold string, inventoryLen int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fold dir: %v", err)
	}
	ls, err := dgstore.Open(dir + "/fold.jsonl")
	if err != nil {
		t.Fatalf("open fold store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	putID := randomLocalMsgID(t)
	// A pre-climb PLAINTEXT solo put (authored by a seller — in the individual tier
	// the solo agent — carrying cleartext content, §541 §6).
	if err := ls.Append(dgstore.Record{
		ID:        putID,
		Sender:    sellerKey,
		Payload:   localPutPayload("reusable go flock file-lock contention test pattern", 4242),
		Tags:      []string{exchange.TagPut, "exchange:content-type:code"},
		Timestamp: time.Now().UnixNano(),
		Origin:    "local",
	}); err != nil {
		t.Fatalf("append solo put: %v", err)
	}
	// The operator's put-accept for it, authored under the pre-P3 OPAQUE LEGACY
	// operator key. Without the wire-alias its Sender != State.OperatorKey and the
	// sender-must-be-operator gate in applySettlePutAccept DROPS it (the put stays
	// pending → never inventory). With the alias it re-attributes to the nostr key
	// and the put becomes ACTIVE inventory.
	if err := ls.Append(dgstore.Record{
		ID:          randomLocalMsgID(t),
		Sender:      legacyKey,
		Payload:     putAcceptPayload461(t),
		Tags:        []string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPutAccept},
		Antecedents: []string{putID},
		Timestamp:   time.Now().UnixNano() + 1,
		Origin:      "local",
	}); err != nil {
		t.Fatalf("append legacy put-accept: %v", err)
	}

	started := make(chan struct{})
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operatorKey,
		OnStarted:         func() { close(started) },
		Logger:            func(string, ...any) {},
	})
	// Register the legacy→nostr wire-alias BEFORE the fold (applyLegacyOperatorAlias
	// is the real serve seam), so the fold re-attributes the legacy operator record.
	if registerAlias {
		applyLegacyOperatorAlias(eng.State(), legacyKey, operatorKey, nil)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("engine fold did not complete within 5s")
	}

	return eng.State().OperatorKey, len(eng.State().Inventory())
}

// TestE2E_SoloToFleetClimb_IdentityStable_NoPlaintextRepublish is the dontguess-461
// climb certification: identity stability (with legacy wire-alias re-attribution)
// AND the zero-plaintext-republish egress fence, through the REAL nostr identity,
// engine fold, climb watermark, and relay Outbox.
func TestE2E_SoloToFleetClimb_IdentityStable_NoPlaintextRepublish(t *testing.T) {
	dir := t.TempDir()

	// --- SOLO: the REAL persisted secp256k1 nostr operator identity (the exact
	//     function serve mints on the FIRST solo run and reuses forever). soloKey is
	//     State.OperatorKey for the individual tier.
	op1, err := loadOrCreateNostrOperatorIdentity(dir)
	if err != nil {
		t.Fatalf("mint solo nostr operator identity: %v", err)
	}
	soloKey := op1.PubKeyHex()

	// A pre-P3 OPAQUE legacy local operator key (distinct from the nostr key) that
	// historical solo operator records were signed under. Any distinct hex works —
	// the wire-alias maps it to the stable nostr key.
	legacyID, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate legacy operator key: %v", err)
	}
	legacyKey := legacyID.PubKeyHex()

	// ── INVARIANT (1a): IDENTITY STABLE ACROSS THE CLIMB. The climb re-loads the
	//    SAME nostr identity (idempotent — NEVER re-mints a competing key). This is
	//    "State.OperatorKey is the SAME secp256k1 key before/after": the fleet serve
	//    derives OperatorPublicKey from this exact call.
	op2, err := loadOrCreateNostrOperatorIdentity(dir)
	if err != nil {
		t.Fatalf("re-load operator identity on climb: %v", err)
	}
	if op2.PubKeyHex() != soloKey {
		t.Fatalf("CLIMB FORKED THE OPERATOR IDENTITY: solo key %s, fleet key %s — up re-minted instead of reusing the persisted secp256k1 key", soloKey, op2.PubKeyHex())
	}

	// ── INVARIANT (1b): NO SENDER MISMATCH — a historical solo operator record
	//    (put-accept) authored under the legacy key RE-ATTRIBUTES via the wire-alias
	//    and folds under State.OperatorKey, so its put becomes ACTIVE inventory.
	opKeyFold, invWithAlias := foldSoloLog(t, dir+"/withalias", soloKey, soloKey, legacyKey, true)
	if opKeyFold != soloKey {
		t.Fatalf("folded engine State.OperatorKey = %s, want the stable solo/fleet key %s", opKeyFold, soloKey)
	}
	if invWithAlias != 1 {
		t.Fatalf("wire-alias re-attribution FAILED: legacy-key put-accept did not fold the put into inventory (got %d entries, want 1) — a historical solo operator record was dropped at the sender-must-be-operator gate", invWithAlias)
	}

	// LOAD-BEARING CONTROL: the SAME log with NO alias registered. The legacy
	// put-accept is dropped (Sender != OperatorKey) → the put stays pending → ZERO
	// inventory. This proves invariant (1b) is caused by the re-attribution, not by
	// the fold accepting the record regardless.
	_, invNoAlias := foldSoloLog(t, dir+"/noalias", soloKey, soloKey, legacyKey, false)
	if invNoAlias != 0 {
		t.Fatalf("NO-ALIAS control folded %d inventory entries, want 0 — the legacy operator record folded WITHOUT the alias, so the re-attribution assertion is vacuous", invNoAlias)
	}

	// ── INVARIANT (2): NO PLAINTEXT REPUBLISH ON THE CLIMB. Seed the pre-climb
	//    PLAINTEXT corpus the individual tier persisted (Origin=local puts inlining
	//    cleartext, §541 §6) into the serve store. One put is authored under the
	//    legacy key to show the fence covers historical records regardless of which
	//    key signed them — the fence is by Origin/position, not by Sender.
	const secret = "SECRET-461-PRECLIMB-SOLO-PLAINTEXT"
	const n = 4
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open serve store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })
	for i := 0; i < n; i++ {
		sender := soloKey
		if i == n-1 {
			sender = legacyKey // a historical record signed under the pre-P3 key
		}
		if err := ls.Append(dgstore.Record{
			ID:        randomLocalMsgID(t),
			Sender:    sender,
			Payload:   localPutPayload(secret+" pre-climb plaintext put variant", 8000+int64(i)),
			Tags:      []string{exchange.TagPut, "exchange:content-type:code"},
			Timestamp: time.Now().UnixNano() + int64(i),
			Origin:    "local",
		}); err != nil {
			t.Fatalf("append pre-climb plaintext put %d: %v", i, err)
		}
	}

	// The REAL climb watermark: reconstructed from the log (no sidecar yet) as the
	// last-plaintext-content local-origin position — here all n puts carry content.
	watermark, err := establishClimbWatermark(climbWatermarkPath(dir), ls)
	if err != nil {
		t.Fatalf("establishClimbWatermark: %v", err)
	}
	if watermark != n {
		t.Fatalf("climb watermark = %d, want %d (the pre-climb plaintext corpus size)", watermark, n)
	}

	ctx := context.Background()

	// The FENCED relay Outbox — built exactly as serve.go's relay-attach wires it
	// (buildRelayWiring + WithClimbWatermark → relay.WithClimbFence), SIGNED BY THE
	// SAME nostr operator identity op2 (op2.PubKeyHex()==soloKey). recordingPublisher
	// is the passive relay-wire observer (defined in the sibling e18d test) — every
	// *identity.Event it records is precisely what the Outbox hands the relay.
	pub := &recordingPublisher{}
	fenced, _, err := buildRelayWiring(ls, op2, soloKey,
		dir+"/events.jsonl.pubcursor", pub, 0, nil, nil,
		WithClimbWatermark(watermark))
	if err != nil {
		t.Fatalf("buildRelayWiring (fenced): %v", err)
	}
	if got := fenced.outbox.Cursor(); got != watermark {
		t.Fatalf("climb fence did not seed the cursor: Cursor()=%d, want the watermark %d", got, watermark)
	}

	// FENCE HOLDS: one full publish pass republishes NOTHING — zero pre-climb
	// plaintext events on the relay wire, and the secret plaintext appears nowhere.
	if err := fenced.outbox.Tick(ctx); err != nil {
		t.Fatalf("fenced Tick: %v", err)
	}
	if got := len(pub.snapshot()); got != 0 {
		t.Fatalf("CLIMB EGRESS FENCE BREACH: %d pre-climb event(s) republished to the relay, want 0 (the solo plaintext corpus must stay local-only on the climb)", got)
	}
	for _, ev := range pub.snapshot() {
		if strings.Contains(ev.Content, secret) {
			t.Fatalf("CONFIDENTIALITY LEAK: pre-climb plaintext (%q) appeared in a published event (kind %d)", secret, ev.Kind)
		}
	}

	// POST-CLIMB EGRESS UNBLOCKED + IDENTITY STABLE ON THE WIRE: append one operator
	// record ABOVE the watermark (a fleet-era match — protocol metadata, no inline
	// content). It MUST publish, signed by the SAME nostr key — proving the fence
	// blocks only the pre-climb corpus AND the fleet operator identity == the solo
	// one on the wire.
	if err := ls.Append(dgstore.Record{
		ID:        randomLocalMsgID(t),
		Sender:    soloKey,
		Payload:   []byte(`{"buy_id":"b1","entry_id":"e1"}`),
		Tags:      []string{exchange.TagMatch},
		Timestamp: time.Now().UnixNano() + 1_000_000,
		Origin:    "local",
	}); err != nil {
		t.Fatalf("append post-climb match: %v", err)
	}
	if err := fenced.outbox.Tick(ctx); err != nil {
		t.Fatalf("fenced Tick after post-climb append: %v", err)
	}
	post := pub.snapshot()
	if len(post) != 1 {
		t.Fatalf("post-climb egress: want exactly 1 published record (the emission above the watermark), got %d — the fence must not block post-climb egress", len(post))
	}
	if post[0].Kind != nostr.KindMatch {
		t.Fatalf("post-climb published event kind = %d, want KindMatch %d", post[0].Kind, nostr.KindMatch)
	}
	if post[0].PubKey != soloKey {
		t.Fatalf("post-climb fleet egress signed by %s, want the stable solo operator key %s — the fleet identity forked from solo on the wire", post[0].PubKey, soloKey)
	}

	// LOAD-BEARING TWIN: same store + corpus, NO fence (fresh cursor, no watermark).
	// It republishes ALL n pre-climb puts, secret plaintext and all — proving the
	// fence assertion above is real, not a vacuous empty-log pass.
	twinPub := &recordingPublisher{}
	twin, _, err := buildRelayWiring(ls, op2, soloKey,
		dir+"/twin.pubcursor", twinPub, 0, nil, nil)
	if err != nil {
		t.Fatalf("buildRelayWiring (twin): %v", err)
	}
	if err := twin.outbox.Tick(ctx); err != nil {
		t.Fatalf("twin Tick: %v", err)
	}
	var twinPuts, twinSecretHits int
	for _, ev := range twinPub.snapshot() {
		if ev.Kind == nostr.KindPut {
			twinPuts++
		}
		if strings.Contains(ev.Content, secret) {
			twinSecretHits++
		}
	}
	if twinPuts != n {
		t.Fatalf("twin (no fence): republished %d put event(s), want %d — the fence assertion is not load-bearing unless the un-fenced twin leaks the corpus", twinPuts, n)
	}
	if twinSecretHits != n {
		t.Fatalf("twin (no fence): the secret plaintext appeared in %d event(s), want %d — the fixture must carry plaintext for the leak to be real", twinSecretHits, n)
	}
}
