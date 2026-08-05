//go:build unix

package procman_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

func TestTerminatePolicyStopsProcessTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	pm := procman.NewProcman()
	cmd, err := pm.NewExecCmd("/bin/sh", procman.Args("-c",
		fmt.Sprintf(`trap '' TERM; sleep 60 & echo $! > %q; wait`, pidFile)),
		procman.WithGracePeriod(100*time.Millisecond),
		procman.WithProcessTreeTermination(),
	)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	descendantPID := waitForPIDFile(t, pidFile)
	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if processExists(descendantPID) {
		_ = syscall.Kill(descendantPID, syscall.SIGKILL)
		t.Fatalf("expected descendant %d to be terminated", descendantPID)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
			if parseErr != nil {
				t.Fatalf("parse descendant PID: %v", parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read descendant PID: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for descendant PID")
	return 0
}

func processExists(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		if err == nil {
			closeParen := strings.LastIndexByte(string(stat), ')')
			if closeParen >= 0 && len(stat) > closeParen+2 && stat[closeParen+2] == 'Z' {
				return false
			}
		}
	}
	return true
}
