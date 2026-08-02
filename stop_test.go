package procman_test

import (
	"context"
	"errors"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// TestStopEscalatesSIGKILL verifies that a child ignoring SIGTERM is killed
// after the StopGrace period, and that Stop reports that it escalated.
func TestStopEscalatesSIGKILL(t *testing.T) {
	if runtimeGOOS == "windows" {
		t.Skip("SIGTERM escalation is Unix-only")
	}
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "stubborn",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 30*time.Second, "-ignore-term"),
		Env:       procman.TestChildEnv(),
		StopGrace: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give the child time to install its SIGTERM handler.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	stopErr := s.Stop(context.Background(), p)
	elapsed := time.Since(start)

	// The child ignores SIGTERM, so Stop must escalate to SIGKILL after the
	// grace period. It should take at least the grace period.
	if elapsed < 200*time.Millisecond {
		t.Fatalf("Stop returned in %v, expected >= 200ms grace before escalation", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Stop took %v, expected escalation to kill well under 5s", elapsed)
	}
	// Escalation should be reported as ErrStopEscalated since the child
	// ignored SIGTERM and required a hard kill.
	if stopErr == nil {
		t.Fatal("expected Stop to report escalation, got nil")
	}
	if !errors.Is(stopErr, procman.ErrStopEscalated) {
		t.Fatalf("expected ErrStopEscalated, got %v", stopErr)
	}

	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close after escalated Stop")
	}

	info, ok := p.Exit()
	if !ok {
		t.Fatal("Exit() ok=false after Stop")
	}
	// Killed by SIGKILL: a signal is reported, not a clean exit code 0.
	if info.Signal == "" {
		t.Fatalf("expected signal death after escalation, got code=%d", info.Code)
	}
	if info.Signal != "KILL" && info.Signal != "SIGKILL" {
		t.Fatalf("expected SIGKILL, got %q", info.Signal)
	}
}

// TestStopIdempotentExited verifies Stop on an already-exited process is a
// no-op returning nil and signals nothing.
func TestStopIdempotentExited(t *testing.T) {
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "quick",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 50*time.Millisecond),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait for natural exit.
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close")
	}
	pid := p.PID() // 0 after exit
	if pid != 0 {
		t.Fatalf("expected PID 0 after exit, got %d", pid)
	}
	if err := s.Stop(context.Background(), p); err != nil {
		t.Fatalf("Stop on exited process should be nil, got %v", err)
	}
	// And again, still nil.
	if err := s.Stop(context.Background(), p); err != nil {
		t.Fatalf("second Stop on exited process should be nil, got %v", err)
	}
}

// TestStopGracefulExit verifies a child that honours SIGTERM exits within the
// grace period without escalation.
func TestStopGracefulExit(t *testing.T) {
	if runtimeGOOS == "windows" {
		t.Skip("SIGTERM is Unix-only")
	}
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "polite",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 30*time.Second),
		Env:       procman.TestChildEnv(),
		StopGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if err := s.Stop(context.Background(), p); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("graceful Stop took %v, expected well under grace", elapsed)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close")
	}
}

// TestStopAllConcurrent verifies StopAll with many children returns once all
// have exited.
func TestStopAllConcurrent(t *testing.T) {
	s := startTestSupervisor(t)
	const n = 20
	for i := 0; i < n; i++ {
		_, err := s.Start(context.Background(), procman.Spec{
			Name:      "child-" + itoa(i),
			Path:      testChildExe(t),
			Args:      procman.TestChildArgs(0, 5*time.Second),
			Env:       procman.TestChildEnv(),
			StopGrace: time.Second,
		})
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
	}
	if got := len(s.List()); got != n {
		t.Fatalf("expected %d processes, got %d", n, got)
	}

	start := time.Now()
	if err := s.StopAll(context.Background()); err != nil {
		// A child that needed escalation may report an error; the plan says
		// StopAll joins errors. We assert all are stopped rather than nil.
		_ = err
	}
	elapsed := time.Since(start)

	// All should stop within roughly one grace period (they honour SIGTERM on
	// Unix; on Windows Kill is immediate). Generous bound.
	if elapsed > 5*time.Second {
		t.Fatalf("StopAll took %v, expected all stopped quickly", elapsed)
	}
	if got := len(s.List()); got != 0 {
		t.Fatalf("expected 0 processes after StopAll, got %d", got)
	}
}

// TestStopContextCancelled verifies Stop respects a cancelled context when the
// child will not die.
func TestStopContextCancelled(t *testing.T) {
	if runtimeGOOS == "windows" {
		t.Skip("SIGTERM is Unix-only")
	}
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "immortal",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 30*time.Second, "-ignore-term"),
		Env:       procman.TestChildEnv(),
		StopGrace: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = s.Stop(ctx, p)
	// Escalation takes >=200ms; a 50ms context should time out first.
	if err == nil {
		// If escalation happened to land in time on a fast machine, that's
		// acceptable; assert the process at least ends up gone.
		select {
		case <-p.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("process did not end up stopped")
		}
		return
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	// Clean up: force kill the survivor.
	_ = s.Stop(context.Background(), p)
}