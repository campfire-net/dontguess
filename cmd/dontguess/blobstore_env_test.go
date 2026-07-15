package main

// blobstore_env_test.go (dontguess-0fd) — the DONTGUESS_BLOSSOM_URL seam that
// wires the seller (put.go), operator (serve.go), and buyer (buy.go) sides of the
// >32 KiB Blossom offload. The full end-to-end proof is the cross-process E2E
// (put_serve_buy_large_0fd_test.go); this pins the fail-open/typed-nil contract
// the three call sites depend on.

import (
	"testing"

	"github.com/3dl-dev/dontguess/pkg/blossom"
)

// TestBlobStoreFromEnv_UnsetIsTrueNil proves an unset (or blank) URL yields a
// TRUE nil interface — never a typed-nil that would slip past the `!= nil` guards
// in serve.go / settle.go and then panic on first use. A typed-nil (*blossom.Client)(nil)
// wrapped in the interface would make `bs != nil` TRUE while every method call NPEs.
func TestBlobStoreFromEnv_UnsetIsTrueNil(t *testing.T) {
	t.Setenv("DONTGUESS_BLOSSOM_URL", "")
	if bs := blobStoreFromEnv(); bs != nil {
		t.Fatalf("blobStoreFromEnv() with unset URL = %#v, want a true nil interface", bs)
	}
	// Whitespace-only must also resolve to nil (the seam TrimSpaces).
	t.Setenv("DONTGUESS_BLOSSOM_URL", "   ")
	if bs := blobStoreFromEnv(); bs != nil {
		t.Fatalf("blobStoreFromEnv() with whitespace URL = %#v, want a true nil interface", bs)
	}
}

// TestBlobStoreFromEnv_SetReturnsBlossomClient proves a configured URL yields a
// live *blossom.Client (the real HTTP transport, not a test double).
func TestBlobStoreFromEnv_SetReturnsBlossomClient(t *testing.T) {
	t.Setenv("DONTGUESS_BLOSSOM_URL", "https://blossom.example.com")
	bs := blobStoreFromEnv()
	if bs == nil {
		t.Fatalf("blobStoreFromEnv() with a URL set = nil, want a *blossom.Client")
	}
	if _, ok := bs.(*blossom.Client); !ok {
		t.Fatalf("blobStoreFromEnv() concrete type = %T, want *blossom.Client", bs)
	}
}
