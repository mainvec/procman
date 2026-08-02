//go:build unix

package procman_test

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// startParentForDeathTest re-executes the test binary as an out-of-process
// parent (PROCMAN_TEST_PARENT=1) that starts a supervised grandchild-bearing
// child, prints PARENT_READY and the grandchild PIDs, then blocks. It returns
// the running *exec.Cmd and the announced grandchild PIDs.
func startParentForDeathTest(t *testing.T, graceSec int, fallback bool) (*exec.Cmd, []int) {
	t.Helper()
	exe, err := procman.TestChildExe()
	if err != nil {
		t.Fatalf("TestChildExe: %v", err)
	}
	args := []string{"-stop-grace=" + itoa(graceSec)}
	if fallback {
		args = append(args, "-fallback")
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(cmd.Env, procman.TestParentEnv()...)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		t.Fatalf("parent start: %v", err)
	}
	_ = w.Close()

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024), 1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	// Wait for PARENT_READY.
	ready := false
	var pids []int
	deadline := time.Now().Add(10 * time.Second)
	for len(pids) < 2 || !ready {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("parent output ended; ready=%v pids=%v", ready, pids)
			}
			if l == "PARENT_READY" {
				ready = true
			} else if strings.HasPrefix(l, "grandchild-") && strings.Contains(l, "pid=") {
				if p := parsePID(l); p > 0 {
					pids = append(pids, p)
				}
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("timed out waiting for parent ready+2 grandchildren; ready=%v pids=%v", ready, pids)
		}
	}
	// Keep the reader goroutine from blocking; r stays open until the parent
	// dies (then EOF). We don't close r here so the parent can still write.
	return cmd, pids
}

// TestParentDeathKillsTree verifies that SIGKILLing the parent kills the child
// and its grandchildren within the grace period, via the /bin/sh watchdog.
func TestParentDeathKillsTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("watchdog sidecar is Unix-only; Windows uses Job Objects (T8)")
	}
	cmd, pids := startParentForDeathTest(t, 2, false)

	// All grandchildren alive before the kill.
	for _, pid := range pids {
		if !processAlive(pid) {
			t.Fatalf("grandchild %d not alive before parent kill", pid)
		}
	}

	// SIGKILL the parent (out-of-band; cannot run a finalizer or stand-down).
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL parent: %v", err)
	}
	_, _ = cmd.Process.Wait()

	// All grandchildren must be gone within a grace-bounded window. The
	// watchdog sees EOF on fd 3, sends SIGTERM, waits StopGrace, then SIGKILL.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		allGone := true
		for _, pid := range pids {
			if processAlive(pid) {
				allGone = false
				break
			}
		}
		if allGone {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	t.Fatalf("grandchildren survived parent SIGKILL; pids=%v", pids)
}

// TestParentDeathKillsTreeFallback repeats the parent-death test with the
// re-exec fallback watchdog (ShellPath pointed at a nonexistent file).
func TestParentDeathKillsTreeFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fallback watchdog is Unix-only")
	}
	cmd, pids := startParentForDeathTest(t, 2, true)

	for _, pid := range pids {
		if !processAlive(pid) {
			t.Fatalf("grandchild %d not alive before parent kill", pid)
		}
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL parent: %v", err)
	}
	_, _ = cmd.Process.Wait()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		allGone := true
		for _, pid := range pids {
			if processAlive(pid) {
				allGone = false
				break
			}
		}
		if allGone {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	t.Fatalf("grandchildren survived parent SIGKILL via fallback; pids=%v", pids)
}

// TestRunWatchdogAndExitNoFd3 verifies that RunWatchdogAndExit / the init hook
// with PROCMAN_WATCHDOG set but no fd 3 exits cleanly and does nothing.
func TestRunWatchdogAndExitNoFd3(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fallback watchdog is Unix-only")
	}
	// Run a fresh process: the test binary re-executed with
	// PROCMAN_WATCHDOG=1 and no fd 3. It must exit 0 quickly.
	exe, err := procman.TestChildExe()
	if err != nil {
		t.Fatalf("TestChildExe: %v", err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "PROCMAN_WATCHDOG=1")
	// No ExtraFiles, so fd 3 is absent.
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("expected clean exit 0 with no fd 3, got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("fallback with no fd 3 took %v, expected immediate exit", d)
	}
}

// TestExitSignalReporting verifies a target killed by SIGSEGV reports
// Signal: "SIGSEGV", not exit code 139. Uses /bin/sleep (a non-Go binary)
// because the Go runtime intercepts SIGSEGV and exits with code 2 rather
// than a signal death.
func TestExitSignalReporting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal reporting is Unix-only")
	}
	s := procman.New(procman.Options{Watchdog: procman.WatchdogOff})
	defer s.Close()
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "segv",
		Path:      "/bin/sleep",
		Args:      []string{"10"},
		StopGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(p.PID(), syscall.SIGSEGV); err != nil {
		t.Fatalf("SIGSEGV: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close after SIGSEGV")
	}
	info, ok := p.Exit()
	if !ok {
		t.Fatal("Exit() ok=false")
	}
	if info.Signal != "SIGSEGV" {
		t.Fatalf("expected Signal %q, got %q (code=%d)", "SIGSEGV", info.Signal, info.Code)
	}
}