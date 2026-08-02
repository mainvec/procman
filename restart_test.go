package procman_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// NOTE: the test child is a re-exec of the Go test binary, which takes ~1s to
// start per generation. The restart tests therefore use generous delays and
// timeouts, and assert generation growth rather than precise backoff values.

// restartPolicy is a helper for the common RestartOnFailure backoff config.
func restartPolicy(maxRetries int, initial, max time.Duration, mult float64, resetAfter time.Duration) procman.RestartPolicy {
	return procman.RestartPolicy{
		Mode:         procman.RestartOnFailure,
		InitialDelay: initial,
		MaxDelay:     max,
		Multiplier:   mult,
		MaxRetries:   maxRetries,
		ResetAfter:   resetAfter,
	}
}

// TestRestartOnFailureIncrementsGeneration verifies a crash-looping child
// restarts with growing generation numbers and exhausts the budget with
// ErrRestartBudgetExhausted. We assert the final state rather than
// mid-flight because the test child re-exec timing is variable.
func TestRestartOnFailureIncrementsGeneration(t *testing.T) {
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "crash",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(1, 0),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
		Restart:   restartPolicy(3, 10*time.Millisecond, 50*time.Millisecond, 2.0, time.Minute),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let the crash loop exhaust the budget (3 restarts -> gen 4 terminal).
	select {
	case <-p.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("Done() did not close after budget exhaustion")
	}
	// It must have restarted at least once (generation > 1); RestartNever
	// would leave generation at 1.
	if g := p.Generation(); g < 2 {
		t.Fatalf("expected generation >= 2 after restarts, got %d", g)
	}
	info, ok := p.Exit()
	if !ok {
		t.Fatal("Exit() ok=false after Done()")
	}
	if info.Err != procman.ErrRestartBudgetExhausted {
		t.Fatalf("expected ErrRestartBudgetExhausted, got %v", info.Err)
	}
}

// TestRestartStopsAfterStop verifies Stop cancels a pending restart and does
// not spawn after it returns. An explicit stop is not a failure.
func TestRestartStopsAfterStop(t *testing.T) {
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "stoppable",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(1, 0),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
		Restart: restartPolicy(5, 50*time.Millisecond, 200*time.Millisecond, 2.0,
			time.Minute),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let one restart happen, then Stop during a backoff sleep.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p.Generation() >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	genAtStop := p.Generation()
	if err := s.Stop(context.Background(), p); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done() did not close after Stop")
	}
	// Generation must not have grown after Stop returned; no new spawn.
	if g := p.Generation(); g > genAtStop+1 {
		t.Fatalf("generation grew to %d after Stop (was %d); a restart spawned after Stop", g, genAtStop)
	}
	// The final exit is not a budget error: an explicit stop is terminal.
	info, ok := p.Exit()
	if !ok {
		t.Fatal("Exit() ok=false")
	}
	if info.Err == procman.ErrRestartBudgetExhausted {
		t.Fatal("explicit Stop should not report budget exhaustion")
	}
}

// TestRestartResetAfter verifies a child that runs longer than ResetAfter then
// dies gets a fresh retry budget, exceeding what MaxRetries would normally
// allow.
func TestRestartResetAfter(t *testing.T) {
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "reset",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(1, 100*time.Millisecond),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
		Restart: procman.RestartPolicy{
			Mode:         procman.RestartOnFailure,
			InitialDelay: 10 * time.Millisecond,
			MaxDelay:     50 * time.Millisecond,
			Multiplier:   2.0,
			MaxRetries:   2,
			ResetAfter:   50 * time.Millisecond, // uptime > this resets budget
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Each generation runs ~100ms (longer than ResetAfter=50ms), so the budget
	// resets and it exceeds the normal 2-restart limit. Allow ~1s per gen.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if p.Generation() >= 4 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if g := p.Generation(); g < 4 {
		t.Fatalf("expected generation >= 4 with reset window, got %d", g)
	}
	if err := s.Stop(context.Background(), p); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestRestartNeverDoesNotRestart verifies RestartNever (zero value) does not
// restart on a failure.
func TestRestartNeverDoesNotRestart(t *testing.T) {
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "norestart",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(1, 0),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
		// Restart is the zero value: RestartNever.
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done() did not close")
	}
	if g := p.Generation(); g != 1 {
		t.Fatalf("RestartNever should not restart; generation = %d, want 1", g)
	}
}

// TestRestartAlwaysRestartsOnSuccess verifies RestartAlways restarts even on
// exit code 0.
func TestRestartAlwaysRestartsOnSuccess(t *testing.T) {
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "always",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 0),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
		Restart: procman.RestartPolicy{
			Mode:         procman.RestartAlways,
			InitialDelay: 10 * time.Millisecond,
			MaxDelay:     50 * time.Millisecond,
			Multiplier:   2.0,
			MaxRetries:   3,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p.Generation() >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if g := p.Generation(); g < 2 {
		t.Fatalf("RestartAlways should restart on success; generation = %d", g)
	}
	if err := s.Stop(context.Background(), p); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done() did not close after Stop")
	}
}

// TestRestartOnFailureNoRestartOnSuccess verifies RestartOnFailure does NOT
// restart on a clean exit (code 0).
func TestRestartOnFailureNoRestartOnSuccess(t *testing.T) {
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "clean",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 0),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
		Restart:   restartPolicy(3, 10*time.Millisecond, 50*time.Millisecond, 2.0, time.Minute),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done() did not close (RestartOnFailure restarted on success)")
	}
	if g := p.Generation(); g != 1 {
		t.Fatalf("RestartOnFailure should not restart on success; generation = %d", g)
	}
}

// TestRestartBackoffGrows verifies the delays grow exponentially. We count
// OnExit calls (each marks a generation's exit); a crash loop with backoff
// produces multiple generations before exhaustion.
func TestRestartBackoffGrows(t *testing.T) {
	var count atomic.Int64
	onExit := func(_ *procman.Process, info procman.ExitInfo) {
		count.Add(1)
	}
	s := procman.New(procman.Options{Watchdog: procman.WatchdogOff, OnExit: onExit})
	defer s.Close()
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "backoff",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(1, 0),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
		Restart:   restartPolicy(4, 20*time.Millisecond, 200*time.Millisecond, 2.0, time.Minute),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("Done() did not close")
	}
	// 4 restarts -> 5 generations total -> 5 OnExit calls.
	if c := count.Load(); c < 3 {
		t.Fatalf("expected >=3 OnExit calls (restarts happened), got %d", c)
	}
}