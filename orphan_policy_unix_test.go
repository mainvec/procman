//go:build unix

package procman_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

func TestTerminatePolicyStopsProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	pm := procman.NewProcman()
	cmd, err := pm.NewExecCmd("/bin/sh", procman.Args("-c",
		fmt.Sprintf(`trap '' TERM; sleep 60 & echo $! > %q; wait`, pidFile)),
		procman.WithGracePeriod(100*time.Millisecond),
		procman.WithProcessTreeTermination(),
	)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	descendantPID := waitForPIDFile(t, pidFile)
	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop waits for the direct child (the shell) to be reaped, not for
	// descendants killed via process-group signal. On a slow CI runner the
	// descendant may still be exiting when Stop returns, so poll briefly
	// instead of checking once.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(descendantPID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(descendantPID, syscall.SIGKILL)
	t.Fatalf("expected descendant %d to be terminated", descendantPID)
}

// startTreeWithStubbornDescendant starts a shell that leaves a descendant
// ignoring SIGTERM, then itself exits on SIGTERM, and returns the descendant's
// PID. The descendant shares the shell's process group.
func startTreeWithStubbornDescendant(t *testing.T, pm *procman.Procman, grace time.Duration) (*procman.ExecCmd, int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	cmd, err := pm.NewExecCmd("/bin/sh", procman.Args("-c",
		fmt.Sprintf(`trap '' TERM; sleep 60 & echo $! > %q; trap - TERM; wait`, pidFile)),
		procman.WithGracePeriod(grace),
		procman.WithProcessTreeTermination(),
	)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := waitForPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return cmd, pid
}

func awaitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected process %d to be terminated", pid)
}

func TestStopKillsDescendantsThatOutliveTheDirectChild(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd, descendantPID := startTreeWithStubbornDescendant(t, pm, 300*time.Millisecond)

	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitProcessGone(t, descendantPID)
}

func TestConcurrentStopWaitsForTheProcessTree(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd, descendantPID := startTreeWithStubbornDescendant(t, pm, time.Second)

	first := make(chan error, 1)
	go func() { first <- cmd.Stop() }()
	select {
	case <-cmd.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("direct child did not exit after SIGTERM")
	}

	// The direct child is reaped; the first Stop is still waiting on the tree.
	if err := cmd.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("first Stop: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("second Stop returned while the first was still waiting for the tree")
	}
	awaitProcessGone(t, descendantPID)
}

func TestOrphanCleanupKillsDescendantsThatIgnoreSIGTERM(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	const grace = 300 * time.Millisecond
	cmd, err := pm.NewExecCmd("/bin/sh", procman.Args("-c",
		fmt.Sprintf(`trap '' TERM; sleep 60 & echo $! > %q`, pidFile)),
		procman.WithProcessTreeTermination(),
		procman.WithOrphanCleanup(),
		procman.WithGracePeriod(grace),
	)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	descendantPID := waitForPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })
	<-cmd.Done()
	exited := time.Now()

	awaitTreeDone(t, cmd, 5*time.Second)
	if elapsed := time.Since(exited); elapsed < grace/2 {
		t.Errorf("tree ended after %v; a descendant ignoring SIGTERM should get the grace period", elapsed)
	}
	awaitProcessGone(t, descendantPID)
	if cmd.StopRequested() {
		t.Error("StopRequested() is true for a command that exited on its own")
	}
}

// A direct child that has died while Wait is still draining its output pipes
// is not alive to the operating system, but its descendants keep the pipes
// open and Done open with them. StopAll used to skip such a command, and
// Shutdown then waited on Done for as long as the descendants lived.
func TestShutdownReachesDescendantsHoldingOutputPipes(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	pm := procman.NewProcman()

	native := exec.Command("/bin/sh", "-c", fmt.Sprintf(`sleep 60 & echo $! > %q`, pidFile))
	var stdout bytes.Buffer
	native.Stdout = &stdout
	cmd, err := pm.NewExecCmdFromCmd(native,
		procman.WithProcessTreeTermination(),
		procman.WithGracePeriod(time.Second),
	)
	if err != nil {
		t.Fatalf("NewExecCmdFromCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	descendantPID := waitForPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })
	awaitProcessGone(t, cmd.Pid())
	select {
	case <-cmd.Done():
		t.Fatal("Done closed while a descendant still holds the output pipe")
	default:
	}

	shutdown := make(chan error, 1)
	go func() { shutdown <- pm.Shutdown() }()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not reach the descendant holding the output pipe")
	}
	awaitProcessGone(t, descendantPID)
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
			if parseErr != nil {
				t.Fatalf("parse descendant PID: %v", parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read descendant PID: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for descendant PID")
	return 0
}

func processExists(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		// ESRCH: reaped between opening the entry and reading it.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return false
		}
		if err == nil {
			closeParen := strings.LastIndexByte(string(stat), ')')
			if closeParen >= 0 && len(stat) > closeParen+2 && stat[closeParen+2] == 'Z' {
				return false
			}
		}
	}
	return true
}
