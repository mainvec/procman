package procman_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// startIgnoringStop starts a command that receives the graceful stop signal
// and keeps running, and waits until its handler is installed.
func startIgnoringStop(t *testing.T, pm *procman.Procman, opts ...procman.ExecCmdOption) *procman.ExecCmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd, err := pm.NewExecCmd(self, procman.Args(testIgnoreStopMode, strconv.Itoa(30), ready), opts...)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cmd
}

func TestStopContextKillsWhenDeadlinePasses(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startIgnoringStop(t, pm) // zero grace: Stop alone would wait forever

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err := cmd.StopContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopContext = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("StopContext returned after %v", elapsed)
	}
	awaitDone(t, cmd)
	if !cmd.StopRequested() {
		t.Error("StopRequested not set")
	}
}

func TestStopContextReturnsStopResultWithinDeadline(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cmd.StopContext(ctx); err != nil {
		t.Fatalf("StopContext: %v", err)
	}
	if !cmd.IsExited() {
		t.Fatal("StopContext returned before the command was reaped")
	}
}

func TestStopContextWithEndedContextKillsAtOnce(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startIgnoringStop(t, pm, procman.WithGracePeriod(time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cmd.StopContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("StopContext = %v, want Canceled", err)
	}
	awaitDone(t, cmd)
}

func TestStopContextEscalatesSharedStop(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startIgnoringStop(t, pm, procman.WithGracePeriod(time.Minute))

	longResult := make(chan error, 1)
	go func() { longResult <- cmd.Stop() }()
	time.Sleep(100 * time.Millisecond) // let Stop begin the shared operation

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := cmd.StopContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short StopContext = %v, want DeadlineExceeded", err)
	}
	select {
	case err := <-longResult:
		if err != nil {
			t.Fatalf("long Stop = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long Stop still waiting after the shared stop was escalated")
	}
}

func TestStopAllContextReportsDeadline(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	a := startIgnoringStop(t, pm)
	b := startIgnoringStop(t, pm, procman.WithProcessTreeTermination())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := pm.StopAllContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopAllContext = %v, want DeadlineExceeded", err)
	}
	awaitDone(t, a)
	awaitDone(t, b)
}

func TestShutdownContextKillsWhenDeadlinePasses(t *testing.T) {
	pm := procman.NewProcman()
	cmd := startIgnoringStop(t, pm, procman.WithProcessTreeTermination())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := pm.ShutdownContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ShutdownContext = %v, want DeadlineExceeded", err)
	}
	awaitDone(t, cmd)
	if _, err := pm.NewExecCmd("x", nil); !errors.Is(err, procman.ErrProcmanShutdown) {
		t.Fatalf("NewExecCmd after ShutdownContext = %v, want ErrProcmanShutdown", err)
	}
	// The escalated run carries on and finishes; a later call returns its result.
	done := make(chan error, 1)
	go func() { done <- pm.Shutdown() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown after escalation = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown run did not finish after escalation")
	}
}

func TestShutdownContextWithinDeadline(t *testing.T) {
	pm := procman.NewProcman()
	name, args := testSleepCommand(t, 30)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pm.ShutdownContext(ctx); err != nil {
		t.Fatalf("ShutdownContext: %v", err)
	}
	if !cmd.IsExited() {
		t.Fatal("ShutdownContext returned before the command was reaped")
	}
}
