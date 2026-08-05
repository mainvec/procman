package procman_test

import (
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

func TestStopRequestedIsFalseForACommandThatEndedOnItsOwn(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testExitCommand(t, 0)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitDone(t, cmd)

	if cmd.StopRequested() {
		t.Error("StopRequested() is true for a command nobody asked to stop")
	}
}

func TestStopRequestedIsTrueAfterStop(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testSleepCommand(t, 30)
	cmd, err := pm.NewExecCmd(name, args, procman.WithGracePeriod(2*time.Second))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitDone(t, cmd)

	if !cmd.StopRequested() {
		t.Error("StopRequested() is false after Stop")
	}
}

// Stopping something that has already died is not the same as having stopped
// it. A supervisor that cannot tell the difference reports every crash it
// happens to tidy up after as an orderly shutdown.
func TestStopRequestedStaysFalseWhenTheCommandWasAlreadyGone(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testExitCommand(t, 1)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitDone(t, cmd)

	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop on an exited command: %v", err)
	}
	if cmd.StopRequested() {
		t.Error("StopRequested() is true for a command that had already exited")
	}
}

func TestStopRequestedIsTrueAfterKillAll(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testSleepCommand(t, 30)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pm.KillAll()
	awaitDone(t, cmd)

	if !cmd.StopRequested() {
		t.Error("StopRequested() is false after KillAll")
	}
}

func TestStopRequestedIsVisibleToTheExitHook(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	var requested bool
	name, args := testSleepCommand(t, 30)
	cmd, err := pm.NewExecCmd(name, args,
		procman.WithGracePeriod(2*time.Second),
		procman.WithOnExit(func(c *procman.ExecCmd, _ error) { requested = c.StopRequested() }),
	)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitDone(t, cmd)

	if !requested {
		t.Error("the exit hook saw StopRequested() = false for a stopped command")
	}
}
