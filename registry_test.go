package procman_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// TestOnExitFiresOncePerGeneration verifies Options.OnExit fires exactly once
// per generation, under concurrent start/stop, with -race. With RestartNever
// there is one generation per process, so OnExit fires once per started
// process.
func TestOnExitFiresOncePerGeneration(t *testing.T) {
	var count int32
	var seen sync.Map
	s := procman.New(procman.Options{
		Watchdog: procman.WatchdogOff,
		OnExit: func(p *procman.Process, info procman.ExitInfo) {
			atomic.AddInt32(&count, 1)
			seen.Store(p.Name(), true)
		},
	})
	defer s.Close()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "p-" + itoa(i)
			p, err := s.Start(context.Background(), procman.Spec{
				Name:      name,
				Path:      testChildExe(t),
				Args:      procman.TestChildArgs(0, 50*time.Millisecond),
				Env:       procman.TestChildEnv(),
				StopGrace: time.Second,
			})
			if err != nil {
				t.Errorf("Start %d: %v", i, err)
				return
			}
			// Stop may race with natural exit; either way OnExit fires once.
			_ = s.Stop(context.Background(), p)
		}(i)
	}
	wg.Wait()

	// Every process eventually reported exactly one OnExit.
	if got := atomic.LoadInt32(&count); got != int32(n) {
		t.Fatalf("OnExit fired %d times, expected %d", got, n)
	}
	// Each name seen exactly once.
	var seenCount int
	seen.Range(func(_, _ interface{}) bool {
		seenCount++
		return true
	})
	if seenCount != n {
		t.Fatalf("seen %d distinct names, expected %d", seenCount, n)
	}
}

// TestOnExitReportsExitInfo verifies the ExitInfo passed to OnExit matches the
// process's final Exit().
func TestOnExitReportsExitInfo(t *testing.T) {
	var got atomic.Pointer[procman.ExitInfo]
	var pname atomic.Value
	s := procman.New(procman.Options{
		Watchdog: procman.WatchdogOff,
		OnExit: func(p *procman.Process, info procman.ExitInfo) {
			got.Store(&info)
			pname.Store(p.Name())
		},
	})
	defer s.Close()

	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "one",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(7, 50*time.Millisecond),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close")
	}
	info := got.Load()
	if info == nil {
		t.Fatal("OnExit did not fire")
	}
	if info.Code != 7 {
		t.Fatalf("OnExit ExitInfo.Code = %d, want 7", info.Code)
	}
	if name, _ := pname.Load().(string); name != "one" {
		t.Fatalf("OnExit process name = %q, want %q", name, "one")
	}
}

// TestCloseIdempotent verifies Close can be called multiple times safely.
func TestCloseIdempotent(t *testing.T) {
	s := procman.New(procman.Options{Watchdog: procman.WatchdogOff})
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("third Close: %v", err)
	}
}

// TestCloseStopsAll verifies Close stops everything.
func TestCloseStopsAll(t *testing.T) {
	s := procman.New(procman.Options{Watchdog: procman.WatchdogOff})
	for i := 0; i < 5; i++ {
		_, err := s.Start(context.Background(), procman.Spec{
			Name:      "c-" + itoa(i),
			Path:      testChildExe(t),
			Args:      procman.TestChildArgs(0, 10*time.Second),
			Env:       procman.TestChildEnv(),
			StopGrace: 200 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(s.List()); got != 0 {
		t.Fatalf("expected 0 processes after Close, got %d", got)
	}
}

// TestStartAfterClose verifies Start on a closed supervisor fails.
func TestStartAfterClose(t *testing.T) {
	s := procman.New(procman.Options{Watchdog: procman.WatchdogOff})
	_ = s.Close()
	_, err := s.Start(context.Background(), procman.Spec{
		Name:      "x",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 0),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
	})
	if err == nil {
		t.Fatal("expected Start to fail on a closed supervisor")
	}
}

// TestGetNotFound verifies Get returns false for an unknown name.
func TestGetNotFound(t *testing.T) {
	s := startTestSupervisor(t)
	if _, ok := s.Get("does-not-exist"); ok {
		t.Fatal("expected Get to return false for unknown name")
	}
}