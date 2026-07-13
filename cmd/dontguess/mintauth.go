package main

// mintauth.go — dontguess-f91 (RT-B#3 disposition, docs/design/nostr-first-client-ed2.md
// §9/§10): the OpMint handler previously authorized a mint on *socket
// reachability alone*. The operator socket lives in a 0700 dir (same-uid local
// access) and is SHARED with OpListHeld/OpAcceptPut/OpRejectPut/OpPut/OpBuy, so
// ANY local process able to connect could trigger eng.MintScrip — an
// operator-signed scrip-mint — without ever proving possession of the operator
// key. This adds an operator-key SIGNATURE requirement inside the OpMint path:
// the request must carry a BIP-340 Schnorr-signed nostr event, authored by the
// persisted operator key, that BINDS the exact recipient+amount being minted.
// Socket reachability is no longer sufficient; proof of the operator key is.
//
// The signed event never touches a relay — it is a local IPC auth token only.
// It is a real nostr Event so verification reuses identity.VerifyEvent (the same
// secp256k1 Schnorr verify the relay Intake uses), not a bespoke crypto path.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/campfire-net/dontguess/pkg/identity"
)

// mintAuthKind is the nostr event kind used for the OpMint IPC auth token. It is
// a LOCAL-ONLY kind (never published to a relay); the value only has to be
// distinct enough to make an auth event self-describing and to prevent a signed
// event of some other kind (e.g. a real put/settle) from being replayed as a
// mint authorization. It sits outside the exchange kind range (3401–3411, 30401).
const mintAuthKind = 27411

// mintAuthRecipientTag / mintAuthAmountTag are the tag names that bind an auth
// event to a specific mint. Both the signer (CLI) and the verifier (serve) use
// these so a captured signed event cannot be reused for a different mint.
const (
	mintAuthRecipientTag = "mint-recipient"
	mintAuthAmountTag     = "mint-amount"
)

// buildMintAuthEvent constructs the UNSIGNED auth event for a mint of `amount`
// scrip to `recipientHex`. The recipient/amount are carried in tags so the
// verifier can confirm the signature covers exactly this mint (SignEvent folds
// the tags into the signed id). createdAt is the caller's clock (Unix seconds);
// it is not enforced for freshness (a same-uid attacker can read the key anyway
// — the gate proves key possession, not timing), but it makes each auth event
// unique.
func buildMintAuthEvent(recipientHex string, amount int64, createdAt int64) *identity.Event {
	return &identity.Event{
		CreatedAt: createdAt,
		Kind:      mintAuthKind,
		Tags: [][]string{
			{mintAuthRecipientTag, strings.ToLower(strings.TrimSpace(recipientHex))},
			{mintAuthAmountTag, strconv.FormatInt(amount, 10)},
		},
		Content: "",
	}
}

// verifyMintAuth is the server-side gate. It rejects the OpMint request unless
// `ev` is a nostr event that:
//
//	(1) is present (a nil auth = an unsigned request → rejected),
//	(2) is of kind mintAuthKind (a signed event of another kind cannot be
//	    replayed as a mint authorization),
//	(3) is authored by the persisted operator key (ev.PubKey == operatorKeyHex),
//	(4) carries a valid BIP-340 Schnorr signature over its id — the REAL crypto
//	    check (identity.VerifyEvent) that makes a forged/stolen pubkey field
//	    insufficient, and
//	(5) binds this exact mint (its recipient/amount tags equal req.Recipient and
//	    req.Amount), so a signed event for one mint cannot be substituted onto a
//	    different recipient or amount.
//
// operatorKeyHex is the exchange's persisted operator public key
// (State().OperatorKey). An empty operator key (individual tier / no persisted
// operator identity) fails closed — mint is a team-tier-only operation.
func verifyMintAuth(ev *identity.Event, operatorKeyHex, recipientHex string, amount int64) error {
	if ev == nil {
		return fmt.Errorf("mint: unsigned request rejected — an operator-key signature is required (socket reachability is not authorization)")
	}
	opKey := strings.ToLower(strings.TrimSpace(operatorKeyHex))
	if opKey == "" {
		return fmt.Errorf("mint: no persisted operator key to verify against (scrip disabled / individual tier)")
	}
	if ev.Kind != mintAuthKind {
		return fmt.Errorf("mint: auth event has wrong kind %d (want %d)", ev.Kind, mintAuthKind)
	}
	// (3) Author must be the operator — reject a wrong-author event before
	// spending a signature verification on it.
	if !strings.EqualFold(strings.TrimSpace(ev.PubKey), opKey) {
		return fmt.Errorf("mint: auth not authored by operator key (pubkey %s != operator %s)",
			shortHex(ev.PubKey), shortHex(opKey))
	}
	// (4) The Schnorr signature (and id integrity) must verify against the
	// claimed pubkey. Only the operator's private key can produce it.
	if err := identity.VerifyEvent(ev); err != nil {
		return fmt.Errorf("mint: operator signature does not verify: %w", err)
	}
	// (5) The signature must cover THIS mint's recipient+amount.
	gotRecipient, gotAmount, ok := mintAuthBinding(ev)
	if !ok {
		return fmt.Errorf("mint: auth event missing recipient/amount binding tags")
	}
	if !strings.EqualFold(gotRecipient, strings.ToLower(strings.TrimSpace(recipientHex))) {
		return fmt.Errorf("mint: auth recipient does not match request (signed for a different recipient)")
	}
	if gotAmount != strconv.FormatInt(amount, 10) {
		return fmt.Errorf("mint: auth amount does not match request (signed for a different amount)")
	}
	return nil
}

// mintAuthBinding extracts the recipient/amount binding tags from an auth event.
func mintAuthBinding(ev *identity.Event) (recipient, amount string, ok bool) {
	var haveR, haveA bool
	for _, t := range ev.Tags {
		if len(t) < 2 {
			continue
		}
		switch t[0] {
		case mintAuthRecipientTag:
			recipient, haveR = t[1], true
		case mintAuthAmountTag:
			amount, haveA = t[1], true
		}
	}
	return recipient, amount, haveR && haveA
}

// loadOperatorSigner loads the persisted operator secp256k1 identity from
// $DG_HOME/nostr-operator.key so the CLI can sign a mint-auth event. It is
// LOAD-ONLY (never creates): if the key file is absent the operator has not run
// `dontguess init`, so minting is not possible and the caller must fail loud
// rather than mint a fresh, non-operator key.
func loadOperatorSigner(dgHome string) (*identity.Secp256k1Identity, error) {
	path := filepath.Join(dgHome, "nostr-operator.key")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no operator key at %s — run `dontguess init` first", path)
		}
		return nil, fmt.Errorf("reading operator key %s: %w", path, err)
	}
	id, err := identity.FromPrivHex(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("parsing operator key %s: %w", path, err)
	}
	return id, nil
}

// shortHex abbreviates a hex key for diagnostics without leaking the full value.
func shortHex(k string) string {
	k = strings.TrimSpace(k)
	if len(k) <= 12 {
		return k
	}
	return k[:8] + "…" + k[len(k)-4:]
}
