// Package scale_test holds shared integration-test helpers.
//
// The campfire-era deployment-mode tests (Mode1/2/3 project/user/team) and the
// cf-wrapper agent-identity tests were removed with the nostr-first cutover:
// they exercised the `dontguess buy/put/match` cf-wrapper client, which no
// longer exists. The nostr-first client (relay-publisher CLI + campfire-free
// tier model + integration tests) is tracked as its own epic. What remains here
// is the ETXTBSY-safe exec helper the surviving install tests depend on.
package scale_test

import (
	"os"
	"syscall"
	"testing"
)

// writeExecFile writes an executable script, closing the ETXTBSY race at its
// source (golang/go#22315). When a Go process writes an executable with a
// plain os.WriteFile and another goroutine concurrently fork+execs (every
// t.Parallel subtest here drives subprocesses), the racing child inherits our
// still-open write fd across the fork and pins the file "text file busy" until
// the child reaches its own execve. Our subsequent exec of the just-written
// script then fails with ETXTBSY — observed as an empty arg log (the wrapper or
// stub never ran). os/exec serializes fd inheritance with syscall.ForkLock
// (read-locked around forkExec); taking the WRITE lock here guarantees no fork
// runs while our write fd is open, so no child can inherit it. Deterministic:
// no retries, no sleeps — the race window is structurally eliminated.
func writeExecFile(t *testing.T, path string, body []byte) error {
	t.Helper()
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
