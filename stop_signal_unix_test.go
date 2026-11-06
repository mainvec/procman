//go:build unix

package procman_test

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

const testExitOnSIGINTMode = "--procman-exit-on-sigint"

func init() {
	if len(os.Args) < 3 || os.Args[1] != testExitOnSIGINTMode {
		return
	}
	// Ignore SIGTERM; exit cleanly on SIGINT. Exit code 0 therefore proves
	// SIGINT arrived, and a kill would show as a signal exit instead.
	signal.Ignore(syscall.SIGTERM)
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, syscall.SIGINT)
	_ = os.WriteFile(os.Args[2], []byte("ready"), 0o600)
	select {
	case <-sigint:
		os.Exit(0)
	case <-time.After(30 * time.Second):
		os.Exit(3)
	}
}

func startExitOnSIGINT(t *testing.T, pm *procman.Procman, opts ...procman.ExecCmdOption) *procman.ExecCmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd, err := pm.NewExecCmd(self, procman.Args(testExitOnSIGINTMode, ready), opts...)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatal("command never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStopSignalReplacesSIGTERM(t *testing.T) {
	for _, tree := range []bool{false, true} {
		name := "direct"
		opts := []procman.ExecCmdOption{procman.WithStopSignal(syscall.SIGINT), procman.WithGracePeriod(10 * time.Second)}
		if tree {
			name = "process-group"
			opts = append(opts, procman.WithProcessTreeTermination())
		}
		t.Run(name, func(t *testing.T) {
			pm := procman.NewProcman()
			t.Cleanup(func() { _ = pm.Shutdown() })
			cmd := startExitOnSIGINT(t, pm, opts...)
			begin := time.Now()
			if err := cmd.Stop(); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if elapsed := time.Since(begin); elapsed > 5*time.Second {
				t.Fatalf("Stop took %v; the child should have exited on SIGINT", elapsed)
			}
			if code := cmd.ExitCode(); code != 0 {
				t.Fatalf("ExitCode = %d, want 0 from the SIGINT handler", code)
			}
		})
	}
}

// Control for the test above: without the option the same child ignores the
// graceful signal and has to be killed.
func TestDefaultStopSignalIsSIGTERM(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startExitOnSIGINT(t, pm, procman.WithGracePeriod(300*time.Millisecond))
	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if code := cmd.ExitCode(); code == 0 {
		t.Fatal("child exited cleanly; it should have ignored SIGTERM and been killed")
	}
}

func TestStopSignalAsManagerDefault(t *testing.T) {
	pm, err := procman.NewProcmanWithOptions(procman.WithDefaultExecCmdOptions(procman.WithStopSignal(syscall.SIGINT)))
	if err != nil {
		t.Fatalf("NewProcmanWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startExitOnSIGINT(t, pm, procman.WithGracePeriod(10*time.Second))
	if err := pm.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if code := cmd.ExitCode(); code != 0 {
		t.Fatalf("ExitCode = %d, want 0 from the SIGINT handler", code)
	}
}

func TestStopSignalRejectsSignalsThatCannotBeHandled(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	for _, sig := range []syscall.Signal{syscall.SIGKILL, syscall.SIGSTOP, 0, -1} {
		if _, err := pm.NewExecCmd("true", nil, procman.WithStopSignal(sig)); err == nil {
			t.Errorf("WithStopSignal(%d) accepted", int(sig))
		} else if errors.Is(err, procman.ErrStopSignalUnsupported) {
			t.Errorf("WithStopSignal(%d) = %v; Unix supports the option", int(sig), err)
		}
	}
}
