package procman_test

import (
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

func TestStartedAtIsZeroUntilTheCommandStarts(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testSleepCommand(t, 30)
	cmd, err := pm.NewExecCmd(name, args, procman.WithGracePeriod(2*time.Second))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if !cmd.StartedAt().IsZero() {
		t.Errorf("StartedAt() = %v before Start, want the zero time", cmd.StartedAt())
	}

	before := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	after := time.Now()
	t.Cleanup(func() { _ = cmd.Stop() })

	startedAt := cmd.StartedAt()
	if startedAt.Before(before) || startedAt.After(after) {
		t.Errorf("StartedAt() = %v, want between %v and %v", startedAt, before, after)
	}
}

// The pair is only worth having if it can be subtracted.
func TestStartedAtPrecedesExitedAt(t *testing.T) {
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

	if !cmd.StartedAt().Before(cmd.ExitedAt()) {
		t.Errorf("StartedAt() = %v is not before ExitedAt() = %v", cmd.StartedAt(), cmd.ExitedAt())
	}
}

func TestExitedAtIsZeroUntilTheCommandHasBeenReaped(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testSleepCommand(t, 30)
	cmd, err := pm.NewExecCmd(name, args, procman.WithGracePeriod(2*time.Second))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if !cmd.ExitedAt().IsZero() {
		t.Errorf("ExitedAt() = %v before Start, want the zero time", cmd.ExitedAt())
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Stop() })

	if !cmd.ExitedAt().IsZero() {
		t.Errorf("ExitedAt() = %v while the command is running, want the zero time", cmd.ExitedAt())
	}
}

func TestExitedAtIsSetOnceTheCommandHasBeenReaped(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testExitCommand(t, 0)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	before := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitDone(t, cmd)
	after := time.Now()

	exitedAt := cmd.ExitedAt()
	if exitedAt.Before(before) || exitedAt.After(after) {
		t.Errorf("ExitedAt() = %v, want between %v and %v", exitedAt, before, after)
	}
}

func TestExitedAtIsVisibleToTheExitHook(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	var seen time.Time
	name, args := testExitCommand(t, 0)
	cmd, err := pm.NewExecCmd(name, args, procman.WithOnExit(func(c *procman.ExecCmd, _ error) {
		seen = c.ExitedAt()
	}))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitDone(t, cmd)

	if seen.IsZero() {
		t.Error("the exit hook saw a zero ExitedAt()")
	}
}
