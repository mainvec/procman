//go:build unix

package procman

import (
	"os/exec"
	"syscall"
	"testing"
)

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
