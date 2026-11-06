//go:build unix

package procman

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// isZombie reports whether pid has died but not been reaped, using ps, which
// both Linux and macOS provide.
func isZombie(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func TestSignalsAfterReapAreNotErrors(t *testing.T) {
	pm := NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	cmd, err := pm.NewExecCmd("/bin/sh", Args("-c", "exit 0"))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// The state Stop is in when the reaper wins the race between Stop's status
	// check and its signal.
	if err := gracefulSignal(cmd); err != nil {
		t.Errorf("gracefulSignal after reap: %v", err)
	}
	if err := forceKill(cmd); err != nil {
		t.Errorf("forceKill after reap: %v", err)
	}
}

// A killed group whose leader has not been reaped yet consists only of zombies.
// macOS answers EPERM to signals for such a group; that is not a failure.
func TestSignalProcessGroupOfZombiesIsNotAnError(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })
	if err := signalProcessGroup(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("signalProcessGroup(live group): %v", err)
	}
	// Leave it unreaped until it has died.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if isZombie(t, pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("process did not die")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := signalProcessGroup(pid, syscall.SIGKILL); err != nil {
		t.Errorf("signalProcessGroup(zombie-only group) = %v, want nil", err)
	}
	if err := signalProcessGroup(pid, syscall.SIGTERM); err != nil {
		t.Errorf("signalProcessGroup(zombie-only group, SIGTERM) = %v, want nil", err)
	}
}

// Once a tree has been observed empty, its group ID may be reused. Simulate
// that by pointing a command at an unrelated live process group: an untracked
// tree must not be signalled, a tracked one must.
func TestSignalTrackedTreeOnlySignalsTrackedTrees(t *testing.T) {
	unrelated := exec.Command("sleep", "30")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatalf("start unrelated group: %v", err)
	}
	t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })

	c := &ExecCmd{
		cmd:                    &exec.Cmd{Process: unrelated.Process},
		processTreeTermination: true,
	}
	if err := c.signalTrackedTree(false); err != nil {
		t.Fatalf("signalTrackedTree on an untracked tree: %v", err)
	}
	if err := syscall.Kill(unrelated.Process.Pid, 0); err != nil {
		t.Fatalf("untracked tree was signalled: %v", err)
	}

	c.treeTracked = true
	if err := c.signalTrackedTree(false); err != nil {
		t.Fatalf("signalTrackedTree on a tracked tree: %v", err)
	}
	if err := unrelated.Wait(); err == nil {
		t.Fatal("tracked tree was not killed")
	}
}
