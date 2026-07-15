package main

// blobstore_env.go (dontguess-0fd) is the single source of the Blossom
// >32 KiB offload/fetch transport shared by all three roles that need it:
//
//   - the SELLER (put.go runPut): offloads the oversize CIPHERTEXT at put time
//     (buildPutMessage, pkg/relayclient) and emits enc.blob_pointer.
//   - the OPERATOR (serve.go runServeLocalCtx): fetch-gates an offloaded
//     blob_pointer put in applyPut (State().SetBlobStore) and re-references it at
//     deliver.
//   - the BUYER (buy.go settle): fetches the offloaded ciphertext at deliver
//     (SettleOptions.BlobStore).
//
// Before 0fd only the buyer side was wired (dontguess-575), so a real
// cross-process >32 KiB FLEET flow could never complete: put.go passed no
// BlobStore (buildPutMessage failed closed) and serve never called
// SetBlobStore (applyPut dropped any blob_pointer put). This helper wires the
// remaining two sides off the SAME DONTGUESS_BLOSSOM_URL env var.

import (
	"os"
	"strings"

	"github.com/3dl-dev/dontguess/pkg/blossom"
	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// blobStoreFromEnv resolves the process Blossom blob store from
// DONTGUESS_BLOSSOM_URL.
//
// Absence is FAIL-OPEN by design: an unset (or blank) DONTGUESS_BLOSSOM_URL
// returns a TRUE nil interface (never a typed-nil), so every ≤32 KiB inline
// path is byte-for-byte unchanged and an oversize operation LOUD-fails at its
// existing nil-store guard — buildPutMessage fail-closed on the seller side
// (pkg/relayclient), applyPut drop on the operator side (state_put.go), and
// settle.go's no-BlobStore error on the buyer side — rather than silently
// degrading. Content-addressing (sha256) means every role that resolves the
// same URL converges on the same pointer for identical ciphertext.
func blobStoreFromEnv() exchange.BlobStore {
	url := strings.TrimSpace(os.Getenv("DONTGUESS_BLOSSOM_URL"))
	if url == "" {
		return nil
	}
	return blossom.NewClient(url)
}
