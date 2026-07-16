package main

// up_lock_test.go — dontguess-647 GROUND-SOURCE.
//
// spawnDetachedServe's check-then-spawn (up.go) previously had no interprocess
// lock: two concurrent `dontguess up` on a cold DG_HOME could both observe the
// operator socket unreachable and both exec a detached `dontguess serve`,
// racing to bind the same unix socket and clobbering the pidfile with the
// loser's PID even though only one serve process actually lives.
//
// This test drives the REAL spawnDetachedServe — including its os.Executable()
// resolution and the REAL exec.Command/cmd.Start() subprocess spawn — from two
// goroutines racing on the same cold DG_HOME. It does not swap in a fake
// upServeLauncher (the runUpCore-level seam other up_test.go tests use); that
// would bypass the exact flock code under test.
//
// The catch: os.Executable() under `go test` resolves to the compiled test
// binary, not the `dontguess` CLI, and that test binary's default entrypoint
// (testing.Main) does not know a "serve" subcommand. So this file supplies a
// TestMain that, when the child process carries the
// DONTGUESS_TEST_FAKE_SERVE=1 marker (inherited via spawnDetachedServe's own
// `cmd.Env = append(os.Environ(), ...)` — no code-under-test changes needed),
// short-circuits straight into a minimal fake "serve": bind the exact unix
// socket path dialSocketMaybe/resolveOperatorSocketPathFor expect, drop a
// per-PID spawn marker file so the test can count how many times a serve
// process actually started, and block until told to stop. This is the
// standard Go "self-reexec helper process" pattern (as used by os/exec's own
// tests) — the subprocess IS the real compiled binary, not a stub swapped in
// for the function under test.
//
// GROUND-SOURCE assertion: two concurrent spawnDetachedServe(dgHome) calls on
// a cold DG_HOME => exactly ONE fake-serve process spawns (one spawn-marker
// file), the pidfile holds that live PID, and the other call observes
// alreadyRunning=true.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

const fakeServeMarkerEnv = "DONTGUESS_TEST_FAKE_SERVE"

func TestMain(m *testing.M) {
	if os.Getenv(fakeServeMarkerEnv) == "1" {
		os.Exit(runFakeServeHelper())
	}
	os.Exit(m.Run())
}

// runFakeServeHelper stands in for `dontguess serve` in the child process
// spawnDetachedServe execs. It binds the same operator unix socket production
// code dials (resolveOperatorSocketPathFor's default: dgHome/ipc/dontguess.sock)
// so dialSocketMaybe genuinely observes it as reachable, drops a per-PID
// marker file under dgHome so the test can count real spawns, and then blocks
// (accepting connections, matching a real listener's behavior) until a bounded
// deadline so no test leaves an orphaned process running indefinitely.
func runFakeServeHelper() int {
	dgHome := os.Getenv("DG_HOME")
	if dgHome == "" {
		return 1
	}
	sockDir := filepath.Join(dgHome, "ipc")
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		return 1
	}
	sockPath := filepath.Join(sockDir, "dontguess.sock")
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return 1
	}
	defer ln.Close()

	marker := filepath.Join(dgHome, fmt.Sprintf("spawn-%d.marker", os.Getpid()))
	if err := os.WriteFile(marker, []byte("spawned\n"), 0o644); err != nil {
		return 1
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = ln.(*net.UnixListener).SetDeadline(time.Now().Add(200 * time.Millisecond))
		conn, aerr := ln.Accept()
		if aerr == nil {
			conn.Close()
		}
	}
	return 0
}

// TestSpawnDetachedServe_ConcurrentLock is the dontguess-647 ground-source
// test: two concurrent real spawnDetachedServe calls on a cold DG_HOME must
// spawn exactly one fake-serve process, and the pidfile must hold that one
// live PID.
func TestSpawnDetachedServe_ConcurrentLock(t *testing.T) {
	dgHome := t.TempDir()

	oldEnv, hadEnv := os.LookupEnv(fakeServeMarkerEnv)
	if err := os.Setenv(fakeServeMarkerEnv, "1"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() {
		if hadEnv {
			_ = os.Setenv(fakeServeMarkerEnv, oldEnv)
		} else {
			_ = os.Unsetenv(fakeServeMarkerEnv)
		}
	})

	oldTimeout := upServeReadyTimeout
	upServeReadyTimeout = 12 * time.Second
	t.Cleanup(func() { upServeReadyTimeout = oldTimeout })

	var wg sync.WaitGroup
	results := make([]struct {
		alreadyRunning bool
		err            error
	}, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ar, err := spawnDetachedServe(dgHome)
			results[i].alreadyRunning = ar
			results[i].err = err
		}(i)
	}
	wg.Wait()

	// Best-effort cleanup: kill whatever fake-serve process is still alive so
	// the test never leaks a background process.
	if pidBytes, rerr := os.ReadFile(pidFilePath(dgHome)); rerr == nil {
		if pid, perr := strconv.Atoi(string(pidBytes)); perr == nil {
			if proc, ferr := os.FindProcess(pid); ferr == nil {
				_ = proc.Signal(syscall.SIGKILL)
			}
		}
	}

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("spawnDetachedServe call %d returned error: %v", i, r.err)
		}
	}

	spawnCount := 0
	winners := 0
	losers := 0
	for _, r := range results {
		if r.alreadyRunning {
			losers++
		} else {
			winners++
		}
	}
	matches, gerr := filepath.Glob(filepath.Join(dgHome, "spawn-*.marker"))
	if gerr != nil {
		t.Fatalf("glob spawn markers: %v", gerr)
	}
	spawnCount = len(matches)

	if spawnCount != 1 {
		t.Fatalf("want exactly 1 real serve spawn, got %d (markers: %v)", spawnCount, matches)
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("want exactly one winner (alreadyRunning=false) and one loser (alreadyRunning=true), got winners=%d losers=%d", winners, losers)
	}

	pidBytes, rerr := os.ReadFile(pidFilePath(dgHome))
	if rerr != nil {
		t.Fatalf("read pidfile: %v", rerr)
	}
	pid, perr := strconv.Atoi(string(pidBytes))
	if perr != nil {
		t.Fatalf("parse pidfile contents %q: %v", pidBytes, perr)
	}
	wantMarker := filepath.Join(dgHome, fmt.Sprintf("spawn-%d.marker", pid))
	if _, serr := os.Stat(wantMarker); serr != nil {
		t.Fatalf("pidfile PID %d does not match the single spawned process's marker %s: %v", pid, wantMarker, serr)
	}
}
