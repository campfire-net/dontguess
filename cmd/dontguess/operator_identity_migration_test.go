package main

// operator_identity_migration_test.go — ground-source E2E for dontguess-f5e /
// Gate A P3 (design §6, ADV-17): ONE secp256k1 operator identity from solo onward,
// with a wire-alias migration for existing opaque-local-key homes.
//
// The proof writes a REAL solo-era log the way a pre-P3 solo `up` did — a plaintext
// put from a participant plus an operator put-accept authored under the opaque
// local-operator.key — then climbs to the relay tier the way runServeLocal now
// does: the stable nostr operator key is State.OperatorKey, a LocalScripStore is
// attached, and the legacy opaque key is registered as a wire-alias via the exact
// serve helper (applyLegacyOperatorAlias → eng.State().RegisterWireAlias). It then
// asserts on the ACTUAL migrated log that:
//   - inventory re-attributes: the solo put-accept folds (via the alias) and the
//     grandfathered plaintext put lands in inventory under the climbed engine;
//   - the migration is load-bearing: the identical climb WITHOUT the alias drops
//     the solo operator records and yields empty inventory (bug reproduction);
//   - assertAdvertiseEqualsSign passes for the single stable identity;
//   - the scrip-store operator gate accepts operator scrip under the nostr key and
//     still rejects a non-operator sender (the migration did not widen the gate);
//   - the on-disk log still carries the legacy Sender — no operator-record re-sign,
//     no relay IO — proving the alias re-attributes at fold time, not by rewriting.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/scrip"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// writeSoloEraLog writes the plaintext put + operator put-accept a pre-P3 solo `up`
// would have produced, with every operator record authored under the opaque legacy
// key. Returns the put id and the seller (participant) key.
func writeSoloEraLog(t *testing.T, ls *dgstore.Store, legacyKey string) (putID, sellerKey string) {
	t.Helper()

	sellerKey = randomLocalMsgID(t)
	putID = randomLocalMsgID(t)
	if err := ls.Append(dgstore.Record{
		ID:         putID,
		CampfireID: "local",
		Sender:     sellerKey,
		Payload:    localPutPayload("Go HTTP handler unit test generator", 8000),
		Tags:       []string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("appending solo put: %v", err)
	}

	// The pre-P3 SOLO engine: opaque legacy key is State.OperatorKey, no ScripStore,
	// no OperatorSigner (individual tier — plaintext, encryptedRequired off). Its
	// AutoAcceptPut emits the operator put-accept with Sender == legacyKey, exactly
	// as a solo home's auto-accept loop did.
	solo := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: legacyKey,
		PollInterval:      20 * time.Millisecond,
		Logger:            func(string, ...any) {},
	})
	if err := solo.AutoAcceptPut(putID, 5600, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("solo AutoAcceptPut: %v", err)
	}
	if got := len(solo.State().Inventory()); got != 1 {
		t.Fatalf("solo-era inventory = %d, want 1 (put-accept did not promote the put)", got)
	}
	return putID, sellerKey
}

// newClimbEngine builds the relay-tier engine the climbed serve wires: the stable
// nostr key is State.OperatorKey, a real LocalScripStore is attached, and an
// OperatorSigner is set (so encryptedRequired is armed — the team tier). withAlias
// registers the legacy opaque key as a wire-alias via the production serve helper.
func newClimbEngine(t *testing.T, ls *dgstore.Store, operator *identity.Secp256k1Identity, legacyKey string, withAlias bool) (*exchange.Engine, *scrip.LocalScripStore) {
	t.Helper()
	nostrKey := operator.PubKeyHex()

	ss, err := scrip.NewLocalScripStore(ls, nostrKey)
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:               "local",
		LocalStore:               ls,
		OperatorPublicKey:        nostrKey,
		OperatorSigner:           operator,
		ScripStore:               ss,
		AutoDeliverOnBuyerAccept: true,
		PollInterval:             20 * time.Millisecond,
		Logger:                   func(string, ...any) {},
	})
	if withAlias {
		// Exercise the ACTUAL production migration wiring, not a hand-rolled alias.
		applyLegacyOperatorAlias(eng.State(), legacyKey, nostrKey, nil)
	}
	if err := eng.StartupReplayForTest(); err != nil {
		t.Fatalf("StartupReplayForTest: %v", err)
	}
	return eng, ss
}

func TestOperatorIdentityMigration_SoloToRelay_WireAliasReattributes(t *testing.T) {
	dgHome := t.TempDir()

	// P3 identity: the single stable secp256k1 nostr operator key (minted at first up).
	operator, err := loadOrCreateNostrOperatorIdentity(dgHome)
	if err != nil {
		t.Fatalf("loadOrCreateNostrOperatorIdentity: %v", err)
	}
	nostrKey := operator.PubKeyHex()

	// A pre-P3 home also has an opaque local-operator.key on disk. Create it exactly
	// as a pre-P3 binary would (loadOrCreateLocalOperatorKey), then read it back the
	// way climbed serve does (loadLegacyLocalOperatorKey).
	if _, err := loadOrCreateLocalOperatorKey(dgHome); err != nil {
		t.Fatalf("seed legacy local-operator.key: %v", err)
	}
	legacyKey, err := loadLegacyLocalOperatorKey(dgHome)
	if err != nil {
		t.Fatalf("loadLegacyLocalOperatorKey: %v", err)
	}
	if legacyKey == "" || legacyKey == nostrKey {
		t.Fatalf("legacy key precondition: legacy=%q nostr=%q", legacyKey, nostrKey)
	}

	ls, err := dgstore.Open(filepath.Join(dgHome, "events.jsonl"))
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	t.Cleanup(func() { ls.Close() }) //nolint:errcheck

	putID, _ := writeSoloEraLog(t, ls, legacyKey)

	// The on-disk operator put-accept must still be authored under the LEGACY key —
	// the migration re-attributes at fold time, it never re-signs the log.
	assertPutAcceptSenderIs(t, ls, putID, legacyKey)

	// --- Bug reproduction: climb WITHOUT the alias drops the solo operator records.
	// The team-tier engine arms encryptedRequired, so the solo plaintext put only
	// survives if its put-accept is recognized (grandfathered). Under the pre-P3
	// two-identity swap the put-accept's Sender (legacyKey) no longer matches
	// State.OperatorKey (nostrKey) → dropped → empty inventory.
	noAlias, _ := newClimbEngine(t, ls, operator, legacyKey, false /* withAlias */)
	if got := len(noAlias.State().Inventory()); got != 0 {
		t.Fatalf("no-alias climb inventory = %d, want 0 (migration should be load-bearing)", got)
	}

	// --- The fix: climb WITH the alias re-attributes the solo operator records.
	climb, ss := newClimbEngine(t, ls, operator, legacyKey, true /* withAlias */)
	inv := climb.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("climbed inventory = %d, want 1 (solo put-accept did not re-attribute via wire-alias)", len(inv))
	}
	if got := climb.State().OperatorKey; got != nostrKey {
		t.Fatalf("climbed State.OperatorKey = %s, want the stable nostr key %s", got, nostrKey)
	}

	// assertAdvertiseEqualsSign: the advertised (config) key and the relay signing
	// key are the SAME single identity, so the ed5 startup gate passes — the climb
	// never triggers the scrip-zeroing mismatch.
	if err := assertAdvertiseEqualsSign(nostrKey, operator.PubKeyHex()); err != nil {
		t.Fatalf("assertAdvertiseEqualsSign after climb: %v", err)
	}

	// Scrip-store operator gate: post-climb operator scrip (Sender == nostrKey) is
	// ACCEPTED and attributed to the stable identity; a non-operator sender is still
	// REJECTED (the migration re-attributes inventory, it does not widen the gate).
	assertScripGate(t, ls, ss, nostrKey)
}

// assertPutAcceptSenderIs fails unless the operator put-accept for putID is still
// authored (on disk) under wantSender — proving no operator-record re-sign.
func assertPutAcceptSenderIs(t *testing.T, ls *dgstore.Store, putID, wantSender string) {
	t.Helper()
	msgs, err := ls.Replay()
	if err != nil {
		t.Fatalf("ls.Replay: %v", err)
	}
	putAcceptPhase := exchange.TagPhasePrefix + exchange.SettlePhaseStrPutAccept
	found := false
	for i := range msgs {
		m := &msgs[i]
		isPutAccept := false
		for _, tag := range m.Tags {
			if tag == putAcceptPhase {
				isPutAccept = true
			}
		}
		if !isPutAccept {
			continue
		}
		found = true
		if m.Sender != wantSender {
			t.Fatalf("put-accept Sender = %s, want legacy %s (migration must NOT re-sign the log)", m.Sender, wantSender)
		}
	}
	if !found {
		t.Fatalf("no operator put-accept found on disk for put %s", putID)
	}
}

// assertScripGate proves the LocalScripStore operator gate accepts operator scrip
// under the stable nostr key and rejects a non-operator sender.
func assertScripGate(t *testing.T, ls *dgstore.Store, ss *scrip.LocalScripStore, nostrKey string) {
	t.Helper()
	ctx := context.Background()

	opRecipient := randomLocalMsgID(t)
	appendScripMint(t, ls, nostrKey /* sender = operator */, opRecipient, 1000)

	forgedRecipient := randomLocalMsgID(t)
	appendScripMint(t, ls, randomLocalMsgID(t) /* sender = non-operator */, forgedRecipient, 500)

	if err := ss.Replay(); err != nil {
		t.Fatalf("scrip store Replay after mints: %v", err)
	}

	if bal, _, err := ss.GetBudget(ctx, opRecipient, scrip.BalanceKey); err != nil || bal != 1000 {
		t.Fatalf("operator-signed scrip: balance=%d err=%v, want 1000 (gate must accept nostr-key scrip)", bal, err)
	}
	if bal, _, err := ss.GetBudget(ctx, forgedRecipient, scrip.BalanceKey); err != nil || bal != 0 {
		t.Fatalf("non-operator scrip: balance=%d err=%v, want 0 (gate must still reject non-operator senders)", bal, err)
	}
}

func appendScripMint(t *testing.T, ls *dgstore.Store, sender, recipient string, amount int64) {
	t.Helper()
	payload, err := json.Marshal(scrip.MintPayload{
		Recipient: recipient,
		Amount:    amount,
		X402TxRef: "migration-test-mint",
		Rate:      1000,
	})
	if err != nil {
		t.Fatalf("marshal mint payload: %v", err)
	}
	if err := ls.Append(dgstore.Record{
		ID:         randomLocalMsgID(t),
		CampfireID: "local",
		Sender:     sender,
		Payload:    payload,
		Tags:       []string{scrip.TagScripMint},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("append scrip mint: %v", err)
	}
}
