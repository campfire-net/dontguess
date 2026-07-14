package exchange

// put_hardening_00d_test.go — the done-gate for dontguess-00d, three §6/§7
// hardening defects found by the dontguess-541 sweep, all in applyPut /
// decryptV2Put (docs/design/content-confidentiality-envelope-541.md §6, §7).
//
// Reuses the REAL v2-envelope + legacy-put builders and teamTierState from
// put_confidentiality_4bed_test.go and putAcceptMsg from
// mixedlog_replay_3ab1_test.go (same package): REAL secp256k1 identities, REAL
// nip44.Seal/Open, REAL ChaCha20-Poly1305 — nothing mocked.
//
// FIX 1: grandfather ONLY pre-cutover plaintext on Replay — a legacy plaintext
//        put with NO operator put-accept in the replayed log (a post-cutover
//        downgrade that was dropped live) must NOT be grandfathered into
//        inventory; a genuine pre-cutover put (WITH a put-accept) still is; the
//        live drop of plaintext stays intact.
// FIX 2: v2 pre-decode size guard + fetched-blob cap.
// FIX 3: reject an inline v2 ciphertext whose plaintext must have offloaded.

import (
	"crypto/rand"
	"strings"
	"testing"

	"github.com/3dl-dev/dontguess/pkg/identity"
)

// ---------------------------------------------------------------------------
// FIX 1 — grandfather is conditioned on a prior operator put-accept.
// ---------------------------------------------------------------------------

// A legacy plaintext put with NO put-accept anywhere in the replayed log is a
// post-cutover downgrade: dropped live, and it must STAY dropped on Replay —
// never grandfathered. A genuine pre-cutover put (WITH a put-accept) still
// grandfathers. The live plaintext drop is unaffected.
func TestReplay_PostCutoverPlaintext_NotGrandfathered_00d(t *testing.T) {
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("seller: %v", err)
	}
	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("operator: %v", err)
	}

	s := teamTierState(t, operator)
	s.OperatorKey = operator.PubKeyHex() // put-accept operator-sender guard ACTIVE

	// (1) A GENUINE PRE-cutover plaintext put: accepted+broadcast before the
	// cutover, so its put-accept is present in the log.
	preCutover := legacyPlaintextPut(t, seller, "pre-cutover accepted go flock contention test pattern",
		[]byte("a pre-migration plaintext artifact that was accepted before the cutover"), 4242)

	// (2) A POST-cutover plaintext put: an allowlisted seller published v1
	// plaintext AFTER the cutover. It was fail-closed DROPPED live, so NO
	// put-accept was ever emitted for it — but the message is durably on the log.
	postCutover := legacyPlaintextPut(t, seller, "post-cutover downgrade runbook migration recipe symlink bridge",
		[]byte("a plaintext put published AFTER the cutover — dropped live, must never grandfather"), 4242)

	log := []*Message{
		preCutover,
		putAcceptMsg(t, operator.PubKeyHex(), preCutover.ID),
		postCutover, // NO put-accept follows — the operator never accepted it
	}
	msgs := make([]Message, len(log))
	for i, m := range log {
		msgs[i] = *m
	}

	s.Replay(msgs)

	// The pre-cutover put (has a put-accept) IS grandfathered into inventory.
	pre := s.GetInventoryEntry(preCutover.ID)
	if pre == nil {
		t.Fatalf("pre-cutover plaintext put (WITH put-accept) was DROPPED on Replay — genuine pre-migration inventory lost")
	}
	if !pre.LegacyPlaintext {
		t.Fatalf("pre-cutover entry not marked LegacyPlaintext — grandfather marker missing")
	}

	// The post-cutover put (NO put-accept) is NOT grandfathered: absent from
	// inventory AND from pendingPuts (never folded at all).
	if e := s.GetInventoryEntry(postCutover.ID); e != nil {
		t.Fatalf("post-cutover plaintext put (NO put-accept) reached INVENTORY on Replay — §6 invariant violated (replay re-admitted a live-dropped plaintext put)")
	}
	if _, ok := s.GetPendingPut(postCutover.ID); ok {
		t.Fatalf("post-cutover plaintext put (NO put-accept) folded into pendingPuts on Replay — the live operator would auto-accept it into inventory")
	}

	// Exactly one live entry: the pre-cutover grandfathered put.
	if n := len(s.Inventory()); n != 1 {
		t.Fatalf("live inventory = %d, want 1 (only the pre-cutover put with a put-accept)", n)
	}
}

// The live drop of a plaintext put (Apply, not Replay) is intact — grandfathering
// is strictly replay-scoped and additionally accept-conditioned, so nothing about
// FIX 1 weakens the live fail-closed guard.
func TestApply_LivePlaintextDrop_StillIntact_00d(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	s := teamTierState(t, operator)
	s.OperatorKey = operator.PubKeyHex()

	live := legacyPlaintextPut(t, seller, "a live plaintext downgrade after the cutover",
		[]byte("brand-new cleartext a rogue client tries to inject live"), 4242)
	s.Apply(live) // Apply, NOT Replay → s.replaying == false → no grandfathering

	if _, ok := s.GetPendingPut(live.ID); ok {
		t.Fatalf("a LIVE plaintext put folded into pendingPuts — live fail-closed enforcement broken")
	}
	if e := s.GetInventoryEntry(live.ID); e != nil {
		t.Fatalf("a LIVE plaintext put reached inventory — grandfathering leaked into the live path")
	}
}

// A forged NON-operator put-accept must NOT bait a post-cutover plaintext put
// into inventory: the Replay pre-scan mirrors applySettlePutAccept's
// operator-sender guard, so a put-accept from a stranger is ignored and the
// plaintext put stays dropped.
func TestReplay_ForgedPutAccept_DoesNotGrandfather_00d(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	stranger, _ := identity.Generate()
	s := teamTierState(t, operator)
	s.OperatorKey = operator.PubKeyHex()

	plain := legacyPlaintextPut(t, seller, "post-cutover put with a FORGED put-accept from a stranger",
		[]byte("plaintext a rogue seller tries to grandfather via a self-signed put-accept"), 4242)
	// A put-accept SIGNED BY A STRANGER (not the operator) — the pre-scan's
	// operator-sender guard must reject it, so this put has NO valid put-accept.
	forged := putAcceptMsg(t, stranger.PubKeyHex(), plain.ID)

	msgs := []Message{*plain, *forged}
	s.Replay(msgs)

	if e := s.GetInventoryEntry(plain.ID); e != nil {
		t.Fatalf("a plaintext put with a FORGED (non-operator) put-accept was grandfathered into inventory — accept-gate trusts a non-operator sender")
	}
	if _, ok := s.GetPendingPut(plain.ID); ok {
		t.Fatalf("a plaintext put with a FORGED put-accept folded into pendingPuts")
	}
}

// ---------------------------------------------------------------------------
// FIX 2 — v2 pre-decode size guard + fetched-blob cap.
// ---------------------------------------------------------------------------

// A v2 inline put whose base64 enc.ciphertext exceeds the pre-decode bound
// (MaxContentBytes*4/3+4) is DROPPED before it is decoded/hashed.
func TestApplyPut_V2Inline_OverCapCiphertext_DroppedBeforeDecode_00d(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	s := teamTierState(t, operator)

	// Start from a well-formed small envelope, then OVERWRITE the inline
	// ciphertext with an over-cap base64 string. All other required fields
	// (content_alg, ciphertext_hash, key_wrap) stay present so encWellFormed
	// passes and the put reaches decryptV2Put — where the pre-decode guard fires
	// BEFORE the base64 decode + sha256.
	enc := buildEncObject(t, seller, operator.PubKeyHex(), []byte("small plaintext"))
	overCap := strings.Repeat("A", MaxContentBytes*4/3+4+1) // valid base64 chars, one past the bound
	enc["ciphertext"] = overCap
	msg := marshalV2Put(t, seller, "an over-cap inline ciphertext DoS attempt", 4242, enc)

	s.Apply(msg)

	if _, ok := s.GetPendingPut(msg.ID); ok {
		t.Fatalf("v2 put with an over-cap inline ciphertext FOLDED — pre-decode size guard did not fire")
	}
}

// A v2 blob_pointer put whose FETCHED bytes exceed MaxContentBytes (a hostile
// Blossom returning gigabytes) is DROPPED before hashing/decrypting.
//
// This case is fold-SENSITIVE to the cap (not merely a re-drop by a later
// check): the ciphertext is a REAL seal with a MATCHING ciphertext_hash and a
// valid CEK, and its plaintext is deliberately JUST UNDER MaxContentBytes (so
// applyPut's own len(contentBytes) > MaxContentBytes gate would PASS it) while
// the ciphertext (plaintext + nonce + tag) is JUST OVER MaxContentBytes. Absent
// the fetched-blob cap, this put would fetch → verify → decrypt → and FOLD; the
// cap is the only thing that drops it — exactly the DoS surface FIX 2 closes.
func TestApplyPut_V2Blob_OverCapFetchedBlob_Dropped_00d(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	s := teamTierState(t, operator)
	store := NewMemoryBlobStore()
	s.SetBlobStore(store)

	// plaintext just under the cap; ciphertext = plaintext + NonceSize(12) +
	// Overhead(16) = plaintext + 28 → just OVER MaxContentBytes.
	plaintext := make([]byte, MaxContentBytes-8) // ciphertext == MaxContentBytes+20
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("gen plaintext: %v", err)
	}
	enc, _ := buildEncObjectBlob(t, seller, operator.PubKeyHex(), plaintext, store)
	// token_cost plausible for this content size (>= MinTokenCost, <= size*MaxTokensPerByte).
	msg := marshalV2Put(t, seller, "a hostile oversize blob fetch just over the content cap", int64(len(plaintext)), enc)

	s.Apply(msg)

	if _, ok := s.GetPendingPut(msg.ID); ok {
		t.Fatalf("v2 blob_pointer put with an over-cap fetched blob FOLDED — fetched-blob cap did not fire")
	}
}

// ---------------------------------------------------------------------------
// FIX 3 — reject an inline v2 ciphertext whose plaintext must have offloaded.
// ---------------------------------------------------------------------------

// A v2 put with an inline ciphertext whose PLAINTEXT exceeds
// BlossomOffloadThreshold must have offloaded (>32 KiB-must-offload storage
// invariant). Inlining it into the replicated message log is DROPPED.
func TestApplyPut_V2Inline_OversizePlaintextMustOffload_Dropped_00d(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	s := teamTierState(t, operator)

	// Plaintext one byte over the offload threshold: a compliant seller MUST
	// offload this; a non-compliant one inlines the ciphertext straight into the
	// log. decoded = nonce||AEAD(plaintext), so len(decoded) exceeds
	// BlossomOffloadThreshold + NonceSize + Overhead → dropped.
	oversize := make([]byte, BlossomOffloadThreshold+1)
	if _, err := rand.Read(oversize); err != nil {
		t.Fatalf("gen oversize plaintext: %v", err)
	}
	enc := buildEncObject(t, seller, operator.PubKeyHex(), oversize)
	msg := marshalV2Put(t, seller, "an oversize inline ciphertext that should have offloaded", 4242, enc)

	s.Apply(msg)

	if _, ok := s.GetPendingPut(msg.ID); ok {
		t.Fatalf("v2 put inlining oversize (>BlossomOffloadThreshold plaintext) ciphertext FOLDED — must-offload storage invariant not enforced")
	}
}

// Guard-boundary regression: a v2 inline put whose plaintext is EXACTLY at
// BlossomOffloadThreshold is still admissible inline (it need not offload) and
// folds — FIX 3 must reject only plaintext ABOVE the threshold.
func TestApplyPut_V2Inline_AtOffloadThreshold_StillFolds_00d(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	s := teamTierState(t, operator)

	atThreshold := make([]byte, BlossomOffloadThreshold)
	if _, err := rand.Read(atThreshold); err != nil {
		t.Fatalf("gen threshold plaintext: %v", err)
	}
	// token_cost must be plausible for this content size (>= size, <= size*MaxTokensPerByte).
	enc := buildEncObject(t, seller, operator.PubKeyHex(), atThreshold)
	msg := marshalV2Put(t, seller, "a substantive at-threshold inline artifact", int64(len(atThreshold)), enc)

	s.Apply(msg)

	entry, ok := s.GetPendingPut(msg.ID)
	if !ok {
		t.Fatalf("v2 put at exactly BlossomOffloadThreshold plaintext was DROPPED — FIX 3 over-rejected the boundary")
	}
	// Sanity: decoded ciphertext is exactly the inline ceiling.
	if entry.ContentSize != int64(BlossomOffloadThreshold) {
		t.Fatalf("entry.ContentSize = %d, want %d", entry.ContentSize, BlossomOffloadThreshold)
	}
}
