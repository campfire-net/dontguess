package main

import "testing"

// TestShouldPrefetchModel covers every branch of the background-prefetch
// decision: default-on, opt-out, under-test suppression, and already-cached.
func TestShouldPrefetchModel(t *testing.T) {
	cases := []struct {
		name        string
		underTest   bool
		noPrefetch  string
		cached      bool
		want        bool
	}{
		{"default on: not cached, prod, no opt-out", false, "", false, true},
		{"suppressed under go test", true, "", false, false},
		{"suppressed by opt-out env", false, "1", false, false},
		{"skip when already cached", false, "", true, false},
		{"opt-out beats not-cached in prod", false, "1", false, false},
		{"cached under test still false", true, "", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldPrefetchModel(c.underTest, c.noPrefetch, c.cached); got != c.want {
				t.Errorf("shouldPrefetchModel(underTest=%v, noPrefetch=%q, cached=%v) = %v, want %v",
					c.underTest, c.noPrefetch, c.cached, got, c.want)
			}
		})
	}
}

// TestMaybePrefetchModel_NoNetworkUnderTest proves the live entrypoint never
// starts a download from `go test`: maybePrefetchModel must return without
// spawning the fetch goroutine (guarded by flag "test.v", which is registered
// in this test binary). If it logged the "prefetching" line, the guard failed.
func TestMaybePrefetchModel_NoNetworkUnderTest(t *testing.T) {
	logged := false
	maybePrefetchModel(func(string, ...any) { logged = true })
	if logged {
		t.Fatal("maybePrefetchModel started a prefetch under `go test` — the test.v guard did not hold")
	}
}
