package main

// serve_relay_large_content_e2e_test.go — dontguess-734, the FLEET LARGE-CONTENT
// ROUND-TRIP release gate (design §7 / §10-Q1: "the ≤32 KiB ceiling is gone").
//
// It proves, END TO END through the REAL client cobra RunE (`dontguess put` /
// `dontguess buy`) + the full team-tier serve stack (real Engine + Intake +
// Outbox + Sequencer + LocalScripStore + TrustChecker + AutoDeliverOnBuyerAccept)
// over the in-process NIP-01 websocket relay, that a put whose plaintext EXCEEDS
// exchange.BlossomOffloadThreshold (32 KiB):
//
//   (1) is OFFLOADED, not inlined — the seller's buildPutMessage stores the AEAD
//       CIPHERTEXT in Blossom and emits enc.blob_pointer; the operator folds it
//       (prefetch → decrypt-to-gate → discard plaintext) into an entry whose
//       Content is nil and whose BlobPointer is set (the >32 KiB invariant); and
//   (2) ROUND-TRIPS byte-exact to a paying buyer — the operator's settle(deliver)
//       carries a ciphertext_ref.blob_pointer (never the bytes), and the buyer's
//       SettleOptions.BlobStore FETCHES the ciphertext from Blossom, verifies
//       sha256(ciphertext)==ciphertext_hash, unwraps the re-wrapped CEK, and
//       AEAD-decrypts. The delivered plaintext equals the >32 KiB input exactly.
//
// GROUND SOURCE / cache-immunity: this is a PACKAGE-MAIN test driving the actual
// RunE against a real serve stack — any edit to put.go / buy.go / serve_relay.go /
// blobstore_env.go invalidates its cache entry, and the TestE2E prefix puts it in
// the named `-count=1` CI acceptance gate.
//
// The Blossom seam is exercised through its PRODUCTION transport: a real
// pkg/blossom.Client (BUD-01/02 HTTP) resolved on ALL THREE sides — seller
// (newSellerBlobStore→DONTGUESS_BLOSSOM_URL), operator (State.SetBlobStore), and
// buyer (newBuyerBlobStore→DONTGUESS_BLOSSOM_URL) — talking to an in-process
// httptest Blossom host. The ONLY fake is the blob HOST itself (a content-addressed
// PUT/GET map, exactly the BUD-01/02 contract a real strfry-adjacent blossom host
// implements); the offload path, the pointer plumbing, the HTTP client, the CEK
// re-wrap, and the hash-verify-before-decrypt are all the real code under test.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/blossom"
	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// blossomTestHost is an in-process, content-addressed BUD-01/02 blob host: PUT
// /{sha256hex} stores the body keyed by the path segment; GET /{sha256hex}
// returns it (404 if unknown). It counts PUTs and GETs so the test can prove the
// SELLER uploaded a blob and BOTH the operator (prefetch) and the buyer (deliver
// fetch) went to Blossom for it — i.e. nothing was inlined.
type blossomTestHost struct {
	srv  *httptest.Server
	mu   sync.Mutex
	blob map[string][]byte
	puts int32
	gets int32
}

func newBlossomTestHost(t *testing.T) *blossomTestHost {
	t.Helper()
	h := &blossomTestHost{blob: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.serve)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *blossomTestHost) serve(w http.ResponseWriter, r *http.Request) {
	ptr := strings.TrimPrefix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodPut:
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(r.Body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.blob[ptr] = buf.Bytes()
		h.mu.Unlock()
		atomic.AddInt32(&h.puts, 1)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		atomic.AddInt32(&h.gets, 1)
		h.mu.Lock()
		body, ok := h.blob[ptr]
		h.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *blossomTestHost) putCount() int { return int(atomic.LoadInt32(&h.puts)) }
func (h *blossomTestHost) getCount() int { return int(atomic.LoadInt32(&h.gets)) }

// largeE2EContent builds a deterministic plaintext comfortably above
// BlossomOffloadThreshold (32 KiB) so the put MUST offload. Deterministic (not
// crypto/rand) so the byte-exact round-trip assertion is reproducible; its text
// relates to the ed2c description so the fold's coherence bookkeeping is realistic,
// though acceptance never depends on it.
func largeE2EContent() []byte {
	var b bytes.Buffer
	for i := 0; b.Len() <= exchange.BlossomOffloadThreshold+8*1024; i++ {
		fmt.Fprintf(&b, "line %06d: package main // generated Go HTTP handler unit test #%d — table-driven, httptest.NewRecorder, asserts status+body\n", i, i)
	}
	return b.Bytes()
}

// shortSHA renders a short content fingerprint for round-trip mismatch
// diagnostics (dumping two 40 KiB blobs into the failure message would be
// unreadable).
func shortSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// TestE2E_TeamLargeContent_BlossomRoundTrip_ClientRunE is the dontguess-734 gate:
// a >32 KiB `dontguess put` offloads its ciphertext to Blossom and a minted
// `dontguess buy` recovers the EXACT plaintext by fetching+verifying+decrypting
// that blob — proving the ≤32 KiB inline ceiling is gone and the buyer BlobStore
// fetch is wired end to end.
func TestE2E_TeamLargeContent_BlossomRoundTrip_ClientRunE(t *testing.T) {
	hushRelayLogs(t)

	blobHost := newBlossomTestHost(t)
	// All three BlobStore sides resolve the SAME content-addressed host. The client
	// sides (seller put, buyer buy) read DONTGUESS_BLOSSOM_URL via blobStoreFromEnv;
	// the operator side is wired explicitly below (newE2EStack does not read env).
	t.Setenv("DONTGUESS_BLOSSOM_URL", blobHost.srv.URL)

	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, sellerHome := newAgentIdentity(t)
	buyer, buyerHome := newAgentIdentity(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	st := newE2EStack(t, ctx, ls, operator, dir+"/events.jsonl.pubcursor",
		e2eShortPublishInterval, seller.PubKeyHex(), buyer.PubKeyHex())
	t.Cleanup(func() { cancel(); st.stop() })

	// Wire the OPERATOR's Blossom seam BEFORE any put is folded (applyPut prefetches
	// the offloaded ciphertext off the lock to decrypt-and-gate it). No put has been
	// injected yet, so this SetBlobStore cannot race an in-flight fold.
	st.eng.State().SetBlobStore(blossom.NewClient(blobHost.srv.URL))

	hub := newE2EHub(t, st.conn)

	largeContent := largeE2EContent()
	if len(largeContent) <= exchange.BlossomOffloadThreshold {
		t.Fatalf("test fixture bug: content is %d bytes, must exceed the %d offload threshold",
			len(largeContent), exchange.BlossomOffloadThreshold)
	}

	// --- allowlisted `dontguess put` RunE with >32 KiB content → offloaded inventory ---
	t.Setenv("AGENT_CF_HOME", sellerHome)
	putCmd := newPutCmd()
	var putOut, putErr bytes.Buffer
	putCmd.SetOut(&putOut)
	putCmd.SetErr(&putErr)
	setPutFlags(t, putCmd, map[string]string{
		"description":   ed2cPutDesc,
		"content":       base64.StdEncoding.EncodeToString(largeContent),
		"token_cost":    "8000",
		"content_type":  "exchange:content-type:code",
		"domains":       "go",
		"relay":         hub.wsURL(),
		"timeout":       "5s",
		"operator-npub": operator.Npub(),
	})
	if err := runPut(putCmd, nil); err != nil {
		t.Fatalf("runPut (large-content allowlisted seller) returned error: %v\nstdout:\n%s\nstderr:\n%s", err, putOut.String(), putErr.String())
	}
	if strings.Contains(putOut.String(), "REJECTED") {
		t.Fatalf("allowlisted large-content put surfaced a spurious REJECTED:\n%s", putOut.String())
	}
	// The seller offloaded the CIPHERTEXT to Blossom during buildPutMessage (a real
	// HTTP PUT) — proving the >32 KiB seller path fired, not the inline path.
	if blobHost.putCount() < 1 {
		t.Fatalf("seller did not upload the ciphertext to Blossom (PUT count %d) — the >32 KiB offload path did not fire", blobHost.putCount())
	}

	waitFor(t, 10*time.Second, "large-content put folds into matchable inventory", func() bool {
		return len(st.eng.State().Inventory()) == 1
	})
	entry := st.eng.State().Inventory()[0]
	if entry.SellerKey != seller.PubKeyHex() {
		t.Fatalf("matchable entry seller = %s, want the allowlisted seller %s", entry.SellerKey, seller.PubKeyHex())
	}
	// THE >32 KiB INVARIANT: the folded entry is a POINTER, not inline bytes. The
	// operator fetched+decrypted the blob to gate it, then DISCARDED the plaintext —
	// Content is nil, BlobPointer is set. This is the concrete proof the ceiling is
	// gone: a >32 KiB entry lives in inventory without ever inlining its bytes.
	if entry.BlobPointer == "" {
		t.Fatalf(">32 KiB entry has no BlobPointer — it was NOT offloaded (ceiling not gone)")
	}
	if entry.Content != nil {
		t.Fatalf(">32 KiB entry inlined %d content bytes — the offload invariant is violated (must be nil)", len(entry.Content))
	}
	// The operator's fold prefetched the ciphertext from Blossom (a real HTTP GET).
	getsAfterFold := blobHost.getCount()
	if getsAfterFold < 1 {
		t.Fatalf("operator did not fetch the offloaded ciphertext from Blossom at fold time (GET count %d)", getsAfterFold)
	}

	// --- minted `dontguess buy` RunE → match → deliver(pointer) → buyer fetch → content ---
	st.mint(t, buyer.PubKeyHex(), wireIDBuyerMint)
	t.Setenv("AGENT_CF_HOME", buyerHome)

	buyCmd := newBuyCmd()
	var buyOut, buyErr bytes.Buffer
	buyCmd.SetOut(&buyOut)
	buyCmd.SetErr(&buyErr)
	setBuyFlags(t, buyCmd, map[string]string{
		"task":    ed2cBuyTask,
		"budget":  "1000000",
		"relay":   hub.wsURL(),
		"timeout": "25s",
	})
	if err := runBuy(buyCmd, nil); err != nil {
		t.Fatalf("runBuy (large-content team hit) returned error: %v\nstderr:\n%s", err, buyErr.String())
	}

	// (a) THE ROUND-TRIP: the delivered plaintext equals the >32 KiB input EXACTLY.
	// The deliver carried only a blob_pointer (emitDeliverEnvelope never inlines the
	// bytes for an offloaded entry), so the ONLY way stdout holds these ~40 KiB is the
	// buyer's SettleOptions.BlobStore fetched the ciphertext from Blossom, verified
	// sha256(ciphertext)==ciphertext_hash, unwrapped the re-wrapped CEK, and decrypted.
	if !bytes.Equal(buyOut.Bytes(), largeContent) {
		t.Fatalf("delivered large content did NOT round-trip byte-exact.\n got %d bytes (sha %s)\nwant %d bytes (sha %s)",
			buyOut.Len(), shortSHA(buyOut.Bytes()), len(largeContent), shortSHA(largeContent))
	}
	if !strings.Contains(buyErr.String(), "SETTLED") {
		t.Fatalf("stderr does not surface the SETTLED outcome:\n%s", buyErr.String())
	}
	// (b) The buyer went to Blossom for the ciphertext (deliver fetch wired): at least
	// one GET occurred AFTER the operator's fold-time prefetch.
	if blobHost.getCount() <= getsAfterFold {
		t.Fatalf("buyer did not fetch the ciphertext from Blossom (GET count %d, was %d after fold) — the buyer BlobStore fetch is not wired",
			blobHost.getCount(), getsAfterFold)
	}

	// (c) REAL scrip moved through the engine — this was a paid sale, not a leak.
	waitFor(t, 8*time.Second, "buyer debited a real price+fee hold", func() bool {
		return st.scrip.Balance(buyer.PubKeyHex()) < wireIDBuyerMint
	})
	matchStore := st.matchStoreID(t)
	waitFor(t, 8*time.Second, "match settles on the client's complete", func() bool {
		return st.eng.State().IsMatchSettled(matchStore)
	})
	waitFor(t, 8*time.Second, "seller credited the residual", func() bool {
		return st.scrip.Balance(seller.PubKeyHex()) > 0
	})
}
