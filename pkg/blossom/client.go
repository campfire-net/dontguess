// Package blossom is the HTTP transport that backs the exchange.BlobStore seam
// (pkg/exchange/blossom.go) for real, cross-process deployments. The exchange
// package ships only the interface plus an in-memory test double
// (MemoryBlobStore); a live team-tier operator and its buyers cannot share an
// in-process map, so oversize (>32 KiB) content must travel through a shared,
// content-addressed HTTP blob host (Blossom, BUD-01/02).
//
// This client is deliberately thin: it is the TRANSPORT, not the fetch/verify
// orchestration. The buyer-side fetch logic (unwrap CEK, verify
// sha256(ciphertext)==ciphertext_hash BEFORE decrypt, AEAD-open) lives in
// pkg/relayclient/settle.go (dontguess-250) and the operator offload-at-put in
// pkg/exchange (dontguess-640). Both consume a BlobStore; this supplies one.
//
// Content-addressing (BUD-01): a blob is addressed by the lowercase hex sha256
// of its bytes. Put(content) uploads to {base}/{sha256hex} and returns that hex
// as the pointer; Fetch(pointer) GETs {base}/{pointer}. Because the address is
// derived from the bytes, Put is idempotent and any node uploading identical
// bytes converges on the same pointer — preserving the deterministic-fold
// invariant the exchange relies on.
//
// The fetch is UNAUTHENTICATED and that is CORRECT (content-confidentiality
// design §2): a Blossom blob is AEAD ciphertext addressed by sha256(ciphertext).
// Fetching it yields only ciphertext, never the CEK (which travels wrapped in
// the deliver's key_wrap). There is nothing to authorize; integrity is enforced
// by the CALLER hashing the fetched bytes against ciphertext_hash before
// decrypt. Do NOT add fetch-auth here.
package blossom

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// Client is a content-addressed HTTP Blossom (BUD-01/02) blob store. It
// implements exchange.BlobStore.
type Client struct {
	baseURL string
	hc      *http.Client
}

// compile-time assertion: the HTTP client satisfies the buyer/operator seam.
var _ exchange.BlobStore = (*Client)(nil)

// NewClient returns a Blossom client rooted at baseURL (e.g.
// "https://blossom.example.com"). A trailing slash is tolerated. The default
// HTTP client carries a bounded timeout so a stalled blob host cannot hang a
// buy past its own settle deadline.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		hc:      &http.Client{Timeout: 60 * time.Second},
	}
}

// NewClientWithHTTP is NewClient with a caller-supplied *http.Client (tests,
// custom transports/timeouts). A nil hc falls back to the default.
func NewClientWithHTTP(baseURL string, hc *http.Client) *Client {
	c := NewClient(baseURL)
	if hc != nil {
		c.hc = hc
	}
	return c
}

// Put uploads content to {base}/{sha256hex} and returns the sha256 hex as the
// pointer. Idempotent for identical bytes.
func (c *Client) Put(content []byte) (string, error) {
	if c.baseURL == "" {
		return "", fmt.Errorf("blossom: no base URL configured")
	}
	sum := sha256.Sum256(content)
	ptr := hex.EncodeToString(sum[:])
	req, err := http.NewRequest(http.MethodPut, c.blobURL(ptr), bytes.NewReader(content))
	if err != nil {
		return "", fmt.Errorf("blossom: build PUT %s: %w", ptr, err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("blossom: PUT blob %s: %w", ptr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("blossom: PUT blob %s: unexpected status %s", ptr, resp.Status)
	}
	return ptr, nil
}

// maxFetchBytes bounds the number of bytes Fetch will read from a blob host
// before rejecting the response (dontguess-4f8). Fetch is deliberately
// UNAUTHENTICATED (see package doc), so a hostile/compromised blob host or a
// network MITM can stream an arbitrary-length response; without a cap,
// io.ReadAll would buffer it all into memory before the caller ever gets a
// chance to check ciphertext_hash — an unauthenticated peer could OOM the
// buyer. The cap is exchange.MaxContentBytes (the largest plaintext a put may
// ever declare) plus generous margin for AEAD framing/nonce/tag overhead and
// Blossom transport padding — legitimate blobs never approach it, so this
// never rejects real content, only unbounded floods.
const maxFetchBytes = exchange.MaxContentBytes + 64*1024 // 1 MiB + 64 KiB margin

// Fetch resolves a pointer (sha256 hex) back to its bytes via GET
// {base}/{pointer}. A 404 maps to exchange.ErrBlobNotFound; any other non-2xx
// or transport failure is a wrapped error. Fetch does NOT verify the bytes
// against the pointer — integrity is the caller's responsibility (it hashes the
// ciphertext against ciphertext_hash before decrypt, settle.go). The read is
// bounded to maxFetchBytes (dontguess-4f8): Fetch is unauthenticated, so a
// hostile host cannot be allowed to stream unbounded bytes into memory before
// that hash check ever runs.
func (c *Client) Fetch(pointer string) ([]byte, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("blossom: no base URL configured")
	}
	pointer = normalizePointer(pointer)
	if pointer == "" {
		return nil, fmt.Errorf("blossom: empty blob pointer")
	}
	resp, err := c.hc.Get(c.blobURL(pointer))
	if err != nil {
		return nil, fmt.Errorf("blossom: GET blob %s: %w", pointer, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("blossom: GET blob %s: %w", pointer, exchange.ErrBlobNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("blossom: GET blob %s: unexpected status %s", pointer, resp.Status)
	}
	// Read one byte past the cap: a legitimate blob at exactly the cap size
	// still round-trips, while any stream that keeps producing bytes past the
	// cap is rejected instead of being read to completion.
	limited := io.LimitReader(resp.Body, maxFetchBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("blossom: GET blob %s: read body: %w", pointer, err)
	}
	if len(body) > maxFetchBytes {
		return nil, fmt.Errorf("blossom: GET blob %s: response exceeds max fetch size (%d bytes)", pointer, maxFetchBytes)
	}
	return body, nil
}

func (c *Client) blobURL(pointer string) string {
	return c.baseURL + "/" + normalizePointer(pointer)
}

// normalizePointer tolerates pointers that carry a scheme-ish prefix (e.g.
// "memblob:<hex>" from the in-memory double, or a full URL) and reduces them to
// the trailing path segment used as the content address. A bare hex digest is
// returned unchanged.
func normalizePointer(pointer string) string {
	p := strings.TrimSpace(pointer)
	// Full URL form: take the last path segment.
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	// scheme:hex form (e.g. memblob:deadbeef): take the part after the last colon.
	if i := strings.LastIndex(p, ":"); i >= 0 {
		p = p[i+1:]
	}
	return p
}
