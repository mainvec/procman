package procman_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// startTestSupervisor builds a Supervisor with sensible defaults for the
// core tests: no watchdog (T3 is the no-watchdog path), no logging.
func startTestSupervisor(t *testing.T) *procman.Supervisor {
	t.Helper()
	s := procman.New(procman.Options{Watchdog: procman.WatchdogOff})
	if s == nil {
		t.Fatal("expected a non-nil Supervisor")
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStartAndExit(t *testing.T) {
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "child",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(3, 200*time.Millisecond),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p == nil {
		t.Fatal("expected a non-nil Process")
	}
	if p.Name() != "child" {
		t.Fatalf("Name = %q, want %q", p.Name(), "child")
	}
	if p.State() != procman.StateRunning && p.State() != procman.StateStarting {
		t.Fatalf("State = %v, want Starting or Running", p.State())
	}
	if p.PID() == 0 {
		t.Fatal("PID should be non-zero after Start")
	}
	if p.Generation() != 1 {
		t.Fatalf("Generation = %d, want 1", p.Generation())
	}

	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close within 5s")
	}

	info, ok := p.Exit()
	if !ok {
		t.Fatal("Exit() ok=false after Done() closed")
	}
	if info.Code != 3 {
		t.Fatalf("Exit code = %d, want 3", info.Code)
	}
	if info.Generation != 1 {
		t.Fatalf("Exit Generation = %d, want 1", info.Generation)
	}
	if p.State() != procman.StateExited {
		t.Fatalf("State = %v, want Exited", p.State())
	}

	// Registry no longer holds it.
	if _, found := s.Get("child"); found {
		t.Fatal("registry still holds exited process")
	}
}

func TestStartExecFailure(t *testing.T) {
	s := startTestSupervisor(t)
	_, err := s.Start(context.Background(), procman.Spec{
		Name:      "bad",
		Path:      "/nonexistent/path/that/does/not/exist",
		Args:      nil,
		StopGrace: time.Second,
	})
	if err == nil {
		t.Fatal("expected Start to fail for a nonexistent path")
	}
	var se *procman.StartError
	if !errors.As(err, &se) {
		t.Fatalf("expected a *StartError, got %T: %v", err, err)
	}
	// An exec failure has no Exit info (the child never ran).
	if se.Exit != nil {
		t.Fatalf("expected nil Exit on exec failure, got %+v", se.Exit)
	}
	if se.Err == nil {
		t.Fatal("expected non-nil Err on StartError")
	}
}

func TestStartDuplicateName(t *testing.T) {
	s := startTestSupervisor(t)
	_, err := s.Start(context.Background(), procman.Spec{
		Name:      "dup",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 5*time.Second),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	_, err = s.Start(context.Background(), procman.Spec{
		Name:      "dup",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 5*time.Second),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
	})
	if err == nil {
		t.Fatal("expected duplicate-name rejection")
	}
	if !errors.Is(err, procman.ErrAlreadyRunning) && !errors.Is(err, procman.ErrDuplicateName) {
		t.Fatalf("expected ErrAlreadyRunning or ErrDuplicateName, got %v", err)
	}
}

// TestConcurrentExitAndStop exercises the single-reaper invariant: Exit() and
// Stop() called concurrently under -race must not race on the wait result.
func TestConcurrentExitAndStop(t *testing.T) {
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "racy",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 300*time.Millisecond),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.Exit()
		}()
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Stop(context.Background(), p)
		}()
	}
	wg.Wait()

	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close after concurrent Exit/Stop")
	}
}