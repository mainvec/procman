//go:build unix

package procman_test

import (
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// Unix-only: os.ProcessState reports -1 for a signalled child. Windows reports
// the termination status the API was given instead.
func TestExitCodeIsNegativeForACommandKilledBySignal(t *testing.T) {
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

	if got := cmd.ExitCode(); got != -1 {
		t.Errorf("ExitCode() = %d for a signalled command, want -1", got)
	}
}
