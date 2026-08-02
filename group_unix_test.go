//go:build unix

package procman_test

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// TestGroupKillTerminatesGrandchildren verifies that Stop kills the whole
// process group, including grandchildren, on Unix. The child spawns two
// long-lived grandchildren; Stop must reach all of them.
func TestGroupKillTerminatesGrandchildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group kill is Unix-only; Windows uses Job Objects (T8)")
	}
	s := startTestSupervisor(t)

	// Capture grandchild PIDs by scanning OnLine output.
	var mu sync.Mutex
	var grandPIDs []int
	onLine := func(l procman.Line) {
		mu.Lock()
		defer mu.Unlock()
		// Grandchild announcements look like "grandchild-N-pid=PID".
		if strings.HasPrefix(l.Text, "grandchild-") {
			if p := parsePID(l.Text); p > 0 {
				grandPIDs = append(grandPIDs, p)
			}
		}
	}
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "tree",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 30*time.Second, "-grandchild=2"),
		Env:       procman.TestChildEnv(),
		StopGrace: 500 * time.Millisecond,
		OnLine:    onLine,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait until both grandchildren have announced.
	if err := waitFor(2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(grandPIDs) >= 2
	}); err != nil {
		t.Fatalf("grandchildren did not announce: %v", err)
	}
	mu.Lock()
	pids := append([]int(nil), grandPIDs...)
	mu.Unlock()

	// Verify grandchildren are alive before Stop.
	for _, pid := range pids {
		if !processAlive(pid) {
			t.Fatalf("grandchild %d not alive before Stop", pid)
		}
	}

	// Stop the parent. With the group-kill seam this should terminate the
	// grandchildren too, not just the parent.
	stopErr := s.Stop(context.Background(), p)
	_ = stopErr

	// All grandchildren must be gone shortly after Stop.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		allGone := true
		for _, pid := range pids {
			if processAlive(pid) {
				allGone = false
				break
			}
		}
		if allGone {
			return // success
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Force-clean any survivors and fail.
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	t.Fatalf("grandchildren survived Stop; pids=%v", pids)
}

// TestGroupKillSIGKILLDirect verifies a direct group SIGKILL via the group seam
// reaps grandchildren when the parent ignores SIGTERM.
func TestGroupKillEscalationReachesGrandchildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group kill is Unix-only")
	}
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
		Name:      "stubborn-tree",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 30*time.Second, "-grandchild=1", "-ignore-term"),
		Env:       procman.TestChildEnv(),
		StopGrace: 200 * time.Millisecond,
		OnLine:    onLine,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := waitFor(2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(grandPIDs) >= 1
	}); err != nil {
		t.Fatalf("grandchild did not announce: %v", err)
	}
	mu.Lock()
	pid := grandPIDs[0]
	mu.Unlock()

	if !processAlive(pid) {
		t.Fatal("grandchild not alive before Stop")
	}

	// The parent ignores SIGTERM, so Stop escalates to group SIGKILL, which
	// must reach the grandchild.
	_ = s.Stop(context.Background(), p)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("grandchild survived escalated group Stop; pid=%d", pid)
}

// keep os referenced for FindProcess fallbacks if needed later.
var _ = os.FindProcess