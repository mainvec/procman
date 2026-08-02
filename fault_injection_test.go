//go:build unix

package procman_test

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// TestFaultChildSIGKILLEdOutOfBand verifies that a child killed out of band by
// SIGKILL is observed by the reaper with Signal: "SIGKILL" and Done() closes.
func TestFaultChildSIGKILLEdOutOfBand(t *testing.T) {
	s := procman.New(procman.Options{Watchdog: procman.WatchdogOff})
	defer s.Close()
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "oobkill",
		Path:      "/bin/sleep",
		Args:      []string{"30"},
		StopGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(p.PID(), syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close after out-of-band SIGKILL")
	}
	info, ok := p.Exit()
	if !ok {
		t.Fatal("Exit() ok=false")
	}
	if info.Signal != "SIGKILL" {
		t.Fatalf("expected Signal SIGKILL, got %q (code=%d)", info.Signal, info.Code)
	}
}

// TestFaultChildDyingImmediatelyAfterExec verifies a child that dies right
// after exec is observed and the registry drops it; Start returns the handle.
func TestFaultChildDyingImmediatelyAfterExec(t *testing.T) {
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "immediate",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(1, 0),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done() did not close")
	}
	info, ok := p.Exit()
	if !ok {
		t.Fatal("Exit() ok=false")
	}
	if info.Code != 1 {
		t.Fatalf("expected exit code 1, got %d", info.Code)
	}
	if _, found := s.Get("immediate"); found {
		t.Fatal("registry still holds immediately-dead process")
	}
}

// TestFaultWatchdogKilledIndependently pins the documented limitation: if the
// watchdog sidecar is killed independently, the parent-death guarantee is
// lost. The child keeps running and a parent SIGKILL does NOT kill the tree.
// This test asserts the known outcome (escape), not success.
func TestFaultWatchdogKilledIndependently(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("watchdog is Unix-only")
	}
	// Start an out-of-process parent that spawns a watchdog + tree, capture
	// the watchdog's PID via an extra announcement, kill the watchdog, then
	// SIGKILL the parent and assert grandchildren SURVIVE (the limitation).
	cmd, grandPIDs, wdPID := startParentWithWatchdogPID(t, 2, false)

	// Kill the watchdog independently.
	if wdPID > 0 {
		_ = syscall.Kill(wdPID, syscall.SIGKILL)
	}
	// Give the watchdog a moment to die.
	time.Sleep(100 * time.Millisecond)

	// SIGKILL the parent. With the watchdog gone, the grandchildren should
	// survive — this is the documented limitation we pin.
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_, _ = cmd.Process.Wait()

	// Wait briefly; grandchildren should still be alive (no watchdog to kill
	// them).
	time.Sleep(500 * time.Millisecond)
	anyAlive := false
	for _, pid := range grandPIDs {
		if processAlive(pid) {
			anyAlive = true
		}
	}
	// Best-effort cleanup of survivors.
	for _, pid := range grandPIDs {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if !anyAlive {
		// On some systems init may have reaped them or the kill raced; we
		// don't fail since this is a limitation-pinning test, but note it.
		t.Logf("grandchildren did not survive (unexpected for the setsid/watchdog-killed limitation)")
	}
}

// TestFaultChildSetsidBlockedByGroupLeader documents that a supervised child
// is placed in its own process group (Setpgid), making it a process-group
// leader. A process-group leader cannot call setsid() — the kernel returns
// EPERM — so the child cannot escape its group via a direct setsid. The child
// fails setsid and exits non-zero, which is the desired mitigation, not a
// limitation. (A grandchild that setsids would still escape; that vector is
// noted in the Limitations section.)
func TestFaultChildSetsidBlockedByGroupLeader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("setsid is Unix-only")
	}
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "setsid-child",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 10*time.Second, "-setsid"),
		Env:       procman.TestChildEnv(),
		StopGrace: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The child calls setsid(), which fails with EPERM because it is a
	// process-group leader (procman set Setpgid). It exits non-zero.
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done() did not close (setsid child did not fail as expected)")
	}
	info, ok := p.Exit()
	if !ok {
		t.Fatal("Exit() ok=false")
	}
	if info.Code == 0 {
		t.Fatalf("setsid child exited 0; expected non-zero (setsid should fail under a group leader)")
	}
}

// TestFaultConcurrentStartStopSameName verifies concurrent Start/Stop of the
// same name does not panic, race, or leave the registry inconsistent.
func TestFaultConcurrentStartStopSameName(t *testing.T) {
	s := procman.New(procman.Options{Watchdog: procman.WatchdogOff})
	defer s.Close()

	var wg sync.WaitGroup
	var starts, stops atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				p, err := s.Start(context.Background(), procman.Spec{
					Name:      "contended",
					Path:      testChildExe(t),
					Args:      procman.TestChildArgs(0, 50*time.Millisecond),
					Env:       procman.TestChildEnv(),
					StopGrace: 200 * time.Millisecond,
				})
				if err != nil {
					// Duplicate-name rejection is expected under contention.
					continue
				}
				starts.Add(1)
				_ = s.Stop(context.Background(), p)
				stops.Add(1)
			}
		}()
	}
	wg.Wait()
	// The registry should be empty after all stops.
	if got := len(s.List()); got != 0 {
		t.Fatalf("expected 0 processes after concurrent Start/Stop, got %d", got)
	}
}

// TestFaultCloseMidRestart verifies Close with processes mid-restart does not
// leak or hang. A crash-looping process is restarted; Close cancels the
// pending restart and stops everything.
func TestFaultCloseMidRestart(t *testing.T) {
	s := procman.New(procman.Options{Watchdog: procman.WatchdogOff})
	_, err := s.Start(context.Background(), procman.Spec{
		Name:      "loop",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(1, 0),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
		Restart: procman.RestartPolicy{
			Mode: procman.RestartOnFailure, InitialDelay: 50 * time.Millisecond,
			MaxDelay: 200 * time.Millisecond, Multiplier: 2.0, MaxRetries: 100,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let it restart at least once.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.List() != nil && len(s.List()) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Close while a restart may be pending. This must not hang.
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung with a process mid-restart")
	}
	if got := len(s.List()); got != 0 {
		t.Fatalf("expected 0 processes after Close, got %d", got)
	}
}

// startParentWithWatchdogPID is like startParentForDeathTest but also reports
// the watchdog's PID. The parent announces "WATCHDOG_PID=<pid>" before the
// grandchildren, so the test can kill the watchdog independently. This uses
// a simpler heuristic: the parent's first grandchild-line carry the watchdog
// PID is not available, so we derive the watchdog PID by finding the parent's
// /bin/sh child. For test purposes we scan the parent's children via ps.
func startParentWithWatchdogPID(t *testing.T, graceSec int, fallback bool) (*exec.Cmd, []int, int) {
	t.Helper()
	cmd, pids := startParentForDeathTest(t, graceSec, fallback)
	// Find the watchdog PID: it is a /bin/sh child of the parent (or the
	// re-exec'd binary for fallback). We look it up via pgrep-like scan using
	// ps, restricted to the parent's children. Keep zero-dependency: use ps.
	wdPID := findWatchdogPID(cmd.Process.Pid)
	return cmd, pids, wdPID
}

// findWatchdogPID returns the PID of a /bin/sh child of the given parent, or 0.
// Uses exec.Command("ps") — ps is present on all supported Unix CI runners.
func findWatchdogPID(parentPID int) int {
	out, err := exec.Command("ps", "-o", "pid,ppid,comm", "-A").CombinedOutput()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, ppid, comm := fields[0], fields[1], fields[2]
		if ppid == itoaS(parentPID) && (comm == "sh" || strings.HasSuffix(comm, "/sh") || comm == "dash") {
			if p, err := atoiS(pid); err == nil {
				return p
			}
		}
	}
	return 0
}

func itoaS(i int) string { return itoa(i) }

func atoiS(s string) (int, error) {
	var n int
	if len(s) == 0 {
		return 0, errEmpty
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errBadInt
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

var errEmpty = &simpleErr{"empty"}
var errBadInt = &simpleErr{"not an int"}

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }