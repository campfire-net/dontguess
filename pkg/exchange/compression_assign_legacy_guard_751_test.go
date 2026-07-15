package exchange

// compression_assign_legacy_guard_751_test.go — the done-gate for dontguess-751,
// defense-in-depth for sendCompressionAssign / sendWarmCompressionAssign
// themselves.
//
// dontguess-9d1 fenced a GRANDFATHERED pre-climb plaintext entry
// (LegacyPlaintext=true) out of findCandidates and added a belt-and-suspenders
// topEntry.LegacyPlaintext skip in emitMatchResponse — so in production these
// helpers are currently UNREACHABLE for a grandfathered entry. But
// sendCompressionAssign and sendWarmCompressionAssign still embed
// entry.ContentHash = sha256(plaintext) into the exchange:assign description
// (compressionProtocol) and only bail early for a v2 confidential entry
// (WrappedCEKOperator != ""), NOT for a grandfathered entry. A future gate
// reorder or a new call site upstream of the existing fences could re-expose the
// same §4.4 A1/P1 plaintext-hash oracle straight out of these helpers.
//
// These are WHITE-BOX (package exchange) tests that call sendCompressionAssign
// and sendWarmCompressionAssign DIRECTLY on a grandfathered entry — bypassing
// findCandidates/emitMatchResponse entirely — proving the helpers themselves now
// refuse to emit anything (and never touch the wire) for a LegacyPlaintext entry,
// while a genuinely-local plaintext CONTROL entry (LegacyPlaintext=false) still
// gets its assign, byte-for-byte unchanged, proving the guard is
// LegacyPlaintext-specific and not a blanket regression.
//
// Reuses egressTestEngine / grandfatheredEntry / localPlaintextEntry / tagPresent
// from climb_fence_grandfather_egress_9d1_test.go (same package, same binary).

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCompressionAssignGuard_LegacyPlaintext_HotPath is the dontguess-751
// done-gate for sendCompressionAssign (the "hot" post-put-accept path): a
// grandfathered entry gets NO exchange:assign and its plaintext hash never
// appears on any emitted message; the control still gets its assign
// byte-for-byte.
func TestCompressionAssignGuard_LegacyPlaintext_HotPath(t *testing.T) {
	t.Parallel()
	eng, ls, operatorKey := egressTestEngine(t)

	gf := grandfatheredEntry("gf-hot", newReservationID(), []byte("pre-migration plaintext whose sha256 must never reach a compression assign"))
	ctrl := localPlaintextEntry("ctrl-hot", newReservationID(), []byte("genuinely-local plaintext that must still get its hot compression assign"))

	if err := eng.sendCompressionAssign(gf); err != nil {
		t.Fatalf("sendCompressionAssign(grandfathered): %v", err)
	}
	if err := eng.sendCompressionAssign(ctrl); err != nil {
		t.Fatalf("sendCompressionAssign(control): %v", err)
	}

	recs, err := ls.ReadAll()
	if err != nil {
		t.Fatalf("ls.ReadAll: %v", err)
	}

	var sawGFAssign, sawCtrlAssign bool
	gfHashHex := strings.TrimPrefix(gf.ContentHash, "sha256:")
	for i := range recs {
		m := &recs[i]
		if m.Sender != operatorKey {
			continue
		}
		if strings.Contains(string(m.Payload), gfHashHex) {
			t.Fatalf("sha256(grandfathered plaintext) found in an emitted message (tags=%v) — plaintext-hash oracle leaked straight out of sendCompressionAssign", m.Tags)
		}
		if !tagPresent(m.Tags, TagAssign) {
			continue
		}
		var ap struct {
			EntryID string `json:"entry_id"`
		}
		if json.Unmarshal(m.Payload, &ap) != nil {
			continue
		}
		switch ap.EntryID {
		case gf.EntryID:
			sawGFAssign = true
		case ctrl.EntryID:
			sawCtrlAssign = true
		}
	}
	if sawGFAssign {
		t.Fatal("sendCompressionAssign emitted an exchange:assign for a grandfathered (LegacyPlaintext) entry — the helper must refuse")
	}
	if !sawCtrlAssign {
		t.Fatal("sendCompressionAssign withheld the assign for the genuinely-local control entry — the fix wrongly touched individual-tier behavior")
	}
}

// TestCompressionAssignGuard_LegacyPlaintext_WarmPath is the same done-gate for
// sendWarmCompressionAssign (the "warm" post-match path directed at the buyer).
func TestCompressionAssignGuard_LegacyPlaintext_WarmPath(t *testing.T) {
	t.Parallel()
	eng, ls, operatorKey := egressTestEngine(t)

	gf := grandfatheredEntry("gf-warm", newReservationID(), []byte("pre-migration plaintext whose sha256 must never reach a warm compression assign"))
	ctrl := localPlaintextEntry("ctrl-warm", newReservationID(), []byte("genuinely-local plaintext that must still get its warm compression assign"))

	if err := eng.sendWarmCompressionAssign(gf, "buyer-key"); err != nil {
		t.Fatalf("sendWarmCompressionAssign(grandfathered): %v", err)
	}
	if err := eng.sendWarmCompressionAssign(ctrl, "buyer-key"); err != nil {
		t.Fatalf("sendWarmCompressionAssign(control): %v", err)
	}

	recs, err := ls.ReadAll()
	if err != nil {
		t.Fatalf("ls.ReadAll: %v", err)
	}

	var sawGFAssign, sawCtrlAssign bool
	gfHashHex := strings.TrimPrefix(gf.ContentHash, "sha256:")
	for i := range recs {
		m := &recs[i]
		if m.Sender != operatorKey {
			continue
		}
		if strings.Contains(string(m.Payload), gfHashHex) {
			t.Fatalf("sha256(grandfathered plaintext) found in an emitted message (tags=%v) — plaintext-hash oracle leaked straight out of sendWarmCompressionAssign", m.Tags)
		}
		if !tagPresent(m.Tags, TagAssign) {
			continue
		}
		var ap struct {
			EntryID string `json:"entry_id"`
		}
		if json.Unmarshal(m.Payload, &ap) != nil {
			continue
		}
		switch ap.EntryID {
		case gf.EntryID:
			sawGFAssign = true
		case ctrl.EntryID:
			sawCtrlAssign = true
		}
	}
	if sawGFAssign {
		t.Fatal("sendWarmCompressionAssign emitted an exchange:assign for a grandfathered (LegacyPlaintext) entry — the helper must refuse")
	}
	if !sawCtrlAssign {
		t.Fatal("sendWarmCompressionAssign withheld the assign for the genuinely-local control entry — the fix wrongly touched individual-tier behavior")
	}
}
