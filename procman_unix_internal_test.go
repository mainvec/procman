//go:build unix

package procman

import "testing"

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
