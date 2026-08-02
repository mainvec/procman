//go:build windows

package procman_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// TestWindowsJobObjectTerminatesTree verifies that Stop terminates a child and
// its grandchildren via the Job Object, not just the single PID.
func TestWindowsJobObjectTerminatesTree(t *testing.T) {
	s := startTestSupervisor(t)

	var mu sync.Mutex
	var grandPIDs []int
	onLine := func(l procman.Line) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(l.Text, "grandchild-") {
			if p := parsePID(l.Text); p > 0 {
				grandPIDs = append(grandPIDs, p)
			}
		}
	}
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "wintree",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 30*time.Second, "-grandchild=2"),
		Env:       procman.TestChildEnv(),
		StopGrace: 2 * time.Second,
		OnLine:    onLine,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := waitFor(3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(grandPIDs) >= 2
	}); err != nil {
		t.Fatalf("grandchildren did not announce: %v", err)
	}
	mu.Lock()
	pids := append([]int(nil), grandPIDs...)
	mu.Unlock()

	// Grandchildren alive before Stop.
	for _, pid := range pids {
		if !processAlive(pid) {
			t.Fatalf("grandchild %d not alive before Stop", pid)
		}
	}

	if err := s.Stop(context.Background(), p); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// All grandchildren must be gone shortly after Stop via the Job Object.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allGone := true
		for _, pid := range pids {
			if processAlive(pid) {
				allGone = false
				break
			}
		}
		if allGone {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchildren survived Stop on Windows; pids=%v", pids)
}