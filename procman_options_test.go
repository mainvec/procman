package procman

import (
	"errors"
	"testing"
	"time"
)

func TestProcmanDefaultExecCmdOptions(t *testing.T) {
	pm, err := NewProcmanWithOptions(
		WithDefaultExecCmdOptions(
			WithGracePeriod(5*time.Second),
			WithProcessTreeTermination(),
			WithParentDeathCleanupIfSupported(),
		),
	)
	if err != nil {
		t.Fatalf("NewProcmanWithOptions: %v", err)
	}

	cmd, err := pm.NewExecCmd("test-command", nil)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if cmd.gracePeriod != 5*time.Second {
		t.Fatalf("expected default grace period 5s, got %v", cmd.gracePeriod)
	}
	if !cmd.processTreeTermination {
		t.Fatal("expected process-tree termination default")
	}
	if cmd.parentDeathCleanup != SupportsParentDeathCleanup() {
		t.Fatalf("parent-death cleanup = %v, support = %v",
			cmd.parentDeathCleanup, SupportsParentDeathCleanup())
	}
}

func TestExecCmdOptionsOverrideProcmanDefaults(t *testing.T) {
	pm, err := NewProcmanWithOptions(
		WithDefaultExecCmdOptions(WithGracePeriod(5 * time.Second)),
	)
	if err != nil {
		t.Fatalf("NewProcmanWithOptions: %v", err)
	}

	cmd, err := pm.NewExecCmd("test-command", nil, WithGracePeriod(time.Second))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if cmd.gracePeriod != time.Second {
		t.Fatalf("expected command grace period 1s, got %v", cmd.gracePeriod)
	}
}

func TestProcmanDefaultExecCmdOptionsValidation(t *testing.T) {
	_, err := NewProcmanWithOptions(
		WithDefaultExecCmdOptions(WithGracePeriod(-time.Second)),
	)
	if err == nil {
		t.Fatal("expected invalid default command option error")
	}
	if errors.Is(err, ErrParentDeathCleanupUnsupported) {
		t.Fatalf("expected grace-period validation error, got %v", err)
	}
}
