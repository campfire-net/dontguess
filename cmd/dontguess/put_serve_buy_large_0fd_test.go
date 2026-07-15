package main

// put_serve_buy_large_0fd_test.go — dontguess-0fd GROUND-SOURCE gate.
//
// dontguess-575 wired only the BUYER side of the >32 KiB Blossom offload; the
// SELLER (put.go) and OPERATOR (serve.go engine) sides were still unwired, so a
// real cross-process oversize FLEET sale could never complete: put.go passed no
// BlobStore to relayclient.Put (buildPutMessage fails closed "no BlobStore
// configured" for >32 KiB) and serve never called State().SetBlobStore (applyPut
// DROPS any blob_pointer put when s.blobStore == nil, state_put.go). 0fd wires
// both off DONTGUESS_BLOSSOM_URL (blobstore_env.go).
//
// This test drives the ACTUAL three roles as three distinct identities against a
// single REAL HTTP Blossom backend, proving >32 KiB content round-trips
// byte-identically END TO END:
//
//   SELLER   real `dontguess put` RunE (runPut) with a >32 KiB plaintext →
//            put.go's newSellerBlobStore() offloads the AEAD CIPHERTEXT to the
//            HTTP backend (PUT) and emits enc.blob_pointer. (If put.go were still
//            unwired, buildPutMessage would fail closed and runPut would error.)
//   OPERATOR the full team-tier serve engine, its State().SetBlobStore wired via
//            the SAME production helper serve.go uses (blobStoreFromEnv). applyPut
//            FETCHES the ciphertext from the backend, verifies ciphertext_hash,
//            decrypts, gates, and auto-accepts into inventory. (If the operator
//            engine were still unwired, applyPut would DROP the put and inventory
//            would never reach 1 — the waitFor below would time out.)
//   BUYER    real `dontguess buy` RunE (runBuy) → match → auto-deliver
//            (blob_pointer) → buy.go's newBuyerBlobStore() FETCHES (GET) the
//            ciphertext, verifies, decrypts → byte-identical plaintext on stdout.
//
// What is REAL vs faked: the crypto (secp256k1 sign, NIP-44 CEK wrap,
// ChaCha20-Poly1305 AEAD), the exchange engine + scrip moves, the nostr
// websocket wire (e2eHub), and the Blossom transport (genuine net/http PUT+GET to
// an httptest server) are all real. The seller offload and buyer fetch are
// UNMOCKED HTTP round trips constructed by the CLI itself from the env — nothing
// about the blob path is stubbed.

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// countingBlossomBackend is a content-addressed HTTP blob host that also counts
// PUTs (seller offloads) and GETs (buyer/operator fetches), so the test can prove
// the ciphertext genuinely traveled over HTTP rather than being inlined or
// resolved from an in-process object.
type countingBlossomBackend struct {
	srv  *httptest.Server
	puts int32
	gets int32
}

func newCountingBlossomBackend(t *testing.T) *countingBlossomBackend {
	t.Helper()
	b := &countingBlossomBackend{}
	var mu sync.Mutex
	blobs := map[string][]byte{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		if key == "" {
			http.Error(w, "no blob address", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPut, http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			mu.Lock()
			blobs[key] = body
			mu.Unlock()
			atomic.AddInt32(&b.puts, 1)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet, http.MethodHead:
			mu.Lock()
			body, ok := blobs[key]
			mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			atomic.AddInt32(&b.gets, 1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(body)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

// TestE2E_0fd_LargeContent_PutServeBuy_CrossProcess_RoundTrips is the item's
// ground-source gate: a >32 KiB entry offloaded by the REAL `dontguess put` and
// fetch-gated by the REAL serve engine (both newly wired) is bought by the REAL
// `dontguess buy` and its decrypted plaintext round-trips byte-for-byte via a
// REAL HTTP Blossom backend.
func TestE2E_0fd_LargeContent_PutServeBuy_CrossProcess_RoundTrips(t *testing.T) {
	hushRelayLogs(t)

	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, sellerHome := newAgentIdentity(t)
	buyer, buyerHome := newAgentIdentity(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	st := newE2EStack(t, ctx, ls, operator, dir+"/events.jsonl.pubcursor",
		e2eShortPublishInterval, seller.PubKeyHex(), buyer.PubKeyHex())
	t.Cleanup(func() { cancel(); st.stop() })
	hub := newE2EHub(t, st.conn)

	// ONE real HTTP Blossom backend. DONTGUESS_BLOSSOM_URL is process-global for
	// the whole test: the seller runPut, the operator engine, and the buyer runBuy
	// all resolve it through the production helpers (blobStoreFromEnv /
	// newSellerBlobStore / newBuyerBlobStore).
	backend := newCountingBlossomBackend(t)
	t.Setenv("DONTGUESS_BLOSSOM_URL", backend.srv.URL)

	// Wire the OPERATOR engine's blob store through the EXACT production helper
	// serve.go's runServeLocalCtx uses (blobStoreFromEnv), reading the env above.
	// Without this the operator's applyPut drops the blob_pointer put and inventory
	// never reaches 1.
	opBlob := blobStoreFromEnv()
	if opBlob == nil {
		t.Fatalf("blobStoreFromEnv() returned nil with DONTGUESS_BLOSSOM_URL=%q — env wiring broken", backend.srv.URL)
	}
	st.eng.State().SetBlobStore(opBlob)

	// A >32 KiB high-entropy plaintext (byte-exact recovery is provable; shares no
	// substring with the public description so a leak is unambiguous).
	block := []byte("DONTGUESS-0fd-LARGE-SECRET-" + randHex(t, 24) + "-" + randHex(t, 96) + " ")
	largeSecret := bytes.Repeat(block, (exchange.BlossomOffloadThreshold/len(block))+64)
	if len(largeSecret) <= exchange.BlossomOffloadThreshold {
		t.Fatalf("large fixture %d not oversize (threshold %d)", len(largeSecret), exchange.BlossomOffloadThreshold)
	}

	const putDesc = "rust tokio async mutex deadlock avoidance concurrency guide"
	const buyTask = "rust tokio async mutex deadlock concurrency avoidance guide"

	// ── SELLER: real `dontguess put` RunE offloads the ciphertext to Blossom ──
	t.Setenv("AGENT_CF_HOME", sellerHome)
	putCmd := newPutCmd()
	var putOut, putErr bytes.Buffer
	putCmd.SetOut(&putOut)
	putCmd.SetErr(&putErr)
	setPutFlags(t, putCmd, map[string]string{
		"description":   putDesc,
		"content":       base64.StdEncoding.EncodeToString(largeSecret),
		"token_cost":    "60000",
		"content_type":  "exchange:content-type:code",
		"domains":       "go",
		"relay":         hub.wsURL(),
		"timeout":       "5s", // allowlisted seller → no put-reject; returns after the window
		"operator-npub": operator.Npub(),
	})
	if err := runPut(putCmd, nil); err != nil {
		t.Fatalf("runPut (large seller offload) returned error: %v\nstdout:\n%s\nstderr:\n%s", err, putOut.String(), putErr.String())
	}
	if strings.Contains(putOut.String(), "REJECTED") {
		t.Fatalf("allowlisted oversize put surfaced a spurious REJECTED:\n%s", putOut.String())
	}
	// The seller MUST have offloaded the ciphertext over HTTP (proves put.go
	// wiring: an unwired put.go fails closed in buildPutMessage before any upload).
	if got := atomic.LoadInt32(&backend.puts); got < 1 {
		t.Fatalf("seller did not offload the ciphertext to Blossom: PUT count=%d, want >=1 (put.go BlobStore not wired?)", got)
	}

	// OPERATOR applyPut must FETCH+verify+gate the offloaded blob and auto-accept
	// it into matchable inventory. If the serve engine's blob store were unwired,
	// applyPut would DROP the blob_pointer put and this would never become 1.
	waitFor(t, 15*time.Second, "operator fetch-gates the offloaded blob_pointer put into inventory", func() bool {
		return len(st.eng.State().Inventory()) == 1
	})
	if got := st.eng.State().Inventory()[0].SellerKey; got != seller.PubKeyHex() {
		t.Fatalf("matchable entry seller = %s, want the allowlisted seller %s", got, seller.PubKeyHex())
	}
	// The operator's fetch is itself an HTTP GET (verify the operator side actually
	// pulled the blob, not merely that the seller pushed one).
	if got := atomic.LoadInt32(&backend.gets); got < 1 {
		t.Fatalf("operator did not fetch the offloaded ciphertext over HTTP: GET count=%d, want >=1 (serve engine SetBlobStore not wired?)", got)
	}

	// ── BUYER: real `dontguess buy` RunE fetches the blob and round-trips ──
	st.mint(t, buyer.PubKeyHex(), wireIDBuyerMint)
	t.Setenv("AGENT_CF_HOME", buyerHome)

	buyCmd := newBuyCmd()
	var buyOut, buyErr bytes.Buffer
	buyCmd.SetOut(&buyOut)
	buyCmd.SetErr(&buyErr)
	setBuyFlags(t, buyCmd, map[string]string{
		"task":    buyTask,
		"budget":  "1000000",
		"relay":   hub.wsURL(),
		"timeout": "60s",
	})
	getsBeforeBuy := atomic.LoadInt32(&backend.gets)
	if err := runBuy(buyCmd, nil); err != nil {
		t.Fatalf("runBuy (large, from CLI) returned error: %v\nstderr:\n%s", err, buyErr.String())
	}

	// (a) content IN HAND, byte-exact, on the pipeable stdout channel.
	if !bytes.Equal(buyOut.Bytes(), largeSecret) {
		t.Fatalf("delivered content mismatch: got %d bytes, want %d bytes (byte-identical required)",
			buyOut.Len(), len(largeSecret))
	}
	if !strings.Contains(buyErr.String(), "SETTLED") {
		t.Fatalf("stderr did not surface the SETTLED outcome:\n%s", buyErr.String())
	}
	// The buyer performed its OWN HTTP GET (a fetch beyond the operator's).
	if got := atomic.LoadInt32(&backend.gets); got <= getsBeforeBuy {
		t.Fatalf("buyer did not fetch the offloaded ciphertext over HTTP: GET count did not advance past %d (buy.go BlobStore not wired?)", getsBeforeBuy)
	}

	// (b) REAL scrip moved through the engine, and the match durably settled.
	waitFor(t, 8*time.Second, "buyer debited a real price+fee hold", func() bool {
		return st.scrip.Balance(buyer.PubKeyHex()) < wireIDBuyerMint
	})
	matchStore := st.matchStoreID(t)
	waitFor(t, 8*time.Second, "match settles (durable scrip-settle) on the client's complete", func() bool {
		return st.eng.State().IsMatchSettled(matchStore)
	})
	waitFor(t, 8*time.Second, "seller credited the residual", func() bool {
		return st.scrip.Balance(seller.PubKeyHex()) > 0
	})
}
