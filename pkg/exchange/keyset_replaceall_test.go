package exchange

import (
	"sync"
	"testing"
)

// TestKeySetReplaceAll_ReplacesNotUnions proves ReplaceAll is the roster fold's
// replace semantics: the prior membership is discarded, so an operator's removal of
// a key takes effect because the new roster simply omits it (design §2/P5).
func TestKeySetReplaceAll_ReplacesNotUnions(t *testing.T) {
	ks := NewKeySet("aaaa", "bbbb")
	if !ks.Allowed("aaaa") || !ks.Allowed("bbbb") {
		t.Fatalf("initial members not admitted")
	}
	// New roster admits only bbbb + cccc — aaaa is REMOVED, cccc is ADDED.
	ks.ReplaceAll("BBBB", " cccc ", "") // mixed case + whitespace + blank
	if ks.Allowed("aaaa") {
		t.Fatalf("aaaa still admitted after ReplaceAll omitted it — union, not replace")
	}
	if !ks.Allowed("bbbb") {
		t.Fatalf("bbbb not admitted (case-insensitive) after ReplaceAll")
	}
	if !ks.Allowed("cccc") {
		t.Fatalf("cccc not admitted (whitespace-trimmed) after ReplaceAll")
	}
	if ks.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (blank entry ignored)", ks.Len())
	}
}

// TestKeySetReplaceAll_Empty proves an empty roster clears membership entirely (the
// last-member-removal case).
func TestKeySetReplaceAll_Empty(t *testing.T) {
	ks := NewKeySet("aaaa")
	ks.ReplaceAll()
	if ks.Len() != 0 || ks.Allowed("aaaa") {
		t.Fatalf("ReplaceAll() did not clear membership: Len=%d", ks.Len())
	}
}

// TestKeySetReplaceAll_Concurrent proves ReplaceAll is race-free against concurrent
// Allowed reads (the folder goroutine swaps while the dispatch/promotion goroutines
// read). Run under -race this fails if the swap is not atomic under the lock.
func TestKeySetReplaceAll_Concurrent(t *testing.T) {
	ks := NewKeySet("aaaa")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = ks.Allowed("aaaa")
				_ = ks.Keys()
			}
		}
	}()
	for i := 0; i < 1000; i++ {
		ks.ReplaceAll("aaaa", "bbbb")
	}
	close(stop)
	wg.Wait()
}
