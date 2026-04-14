package main

// serve_flag_test.go — regression test for dontguess-ba9 (flag default verification).

import "testing"

// TestServeAutoAcceptMaxDefault verifies that the --auto-accept-max-price flag
// default is 1,000,000 tokens. This test exists to catch silent regressions where
// someone changes the default back to an older value (e.g. 100,000) without
// updating the constant and this test.
//
// If this test fails, update DefaultAutoAcceptMax in serve.go AND the flag
// registration in init() to match.
func TestServeAutoAcceptMaxDefault(t *testing.T) {
	const want = int64(1_000_000)
	if DefaultAutoAcceptMax != want {
		t.Errorf("DefaultAutoAcceptMax = %d, want %d — update serve.go if you intentionally changed the default",
			DefaultAutoAcceptMax, want)
	}
}
