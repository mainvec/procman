//go:build windows

// These tests verify Windows-specific parent-death cleanup. The parent-death
// test spawns a child process of the test binary, starts managed grandchildren
// with WithParentDeathCleanup, kills the parent, and verifies that the kernel
// terminates the grandchildren via the kill-on-close Job Object.

package procman_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	procman "github.com/mainvec/procman"
)

// TestWindowsParentDeath verifies that children with WithParentDeathCleanup
// are terminated by the kernel when the parent process is killed. It spawns
// a helper process (the test binary in helper mode) that starts 3 children
// with parent-death cleanup, writes their PIDs to a file, and blocks. The
// test then kills the helper and checks whether the children die.
func TestWindowsParentDeath(t *testing.T) {
	testBinary, _ := os.Executable()
	helper := exec.Command(testBinary, "-test.run=^TestWindowsParentDeathHelper$")
	helper.Env = append(os.Environ(), "PROCMAN_WIN_HELPER=1")
	helper.Stdout = os.Stdout
	helper.Stderr = os.Stderr
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	parentPID := helper.Process.Pid
	t.Logf("helper started, pid=%d", parentPID)

	pidFile := filepath.Join(os.TempDir(), "mainvec_child_pids.txt")
	os.Remove(pidFile)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read PID file: %v", err)
	}

	childPIDs := parseWindowsPIDs(string(data))
	if len(childPIDs) == 0 {
		t.Fatal("no child PIDs")
	}
	t.Logf("child PIDs: %v", childPIDs)

	for _, pid := range childPIDs {
		if !processAliveWin(pid) {
			t.Fatalf("child %d not alive before parent death", pid)
		}
	}
	t.Log("all children alive")

	t.Logf("killing parent pid=%d", parentPID)
	helper.Process.Kill()
	helper.Wait()

	deadline = time.Now().Add(5 * time.Second)
	allDead := false
	for time.Now().Before(deadline) {
		allDead = true
		for _, pid := range childPIDs {
			if processAliveWin(pid) {
				allDead = false
				break
			}
		}
		if allDead {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !allDead {
		for _, pid := range childPIDs {
			if processAliveWin(pid) {
				t.Errorf("child %d STILL ALIVE after parent killed", pid)
			}
		}
	} else {
		t.Log("SUCCESS: all children died after parent was killed")
	}

	for _, pid := range childPIDs {
		killProcessWin(pid)
	}
	os.Remove(pidFile)
}

// TestWindowsParentDeathHelper runs inside the test binary when
// PROCMAN_WIN_HELPER=1 is set. It starts 3 children with
// WithParentDeathCleanup, writes their PIDs to a file, and blocks forever.
func TestWindowsParentDeathHelper(t *testing.T) {
	if os.Getenv("PROCMAN_WIN_HELPER") != "1" {
		t.Skip("not helper mode")
	}

	pm := procman.NewProcman()
	self, _ := os.Executable()

	for i := range 3 {
		cmd, err := pm.NewExecCmd(self,
			procman.Args(testSleepMode, "120"),
			procman.WithParentDeathCleanup(),
		)
		if err != nil {
			t.Fatalf("NewExecCmd %d: %v", i, err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
	}

	time.Sleep(1 * time.Second)

	pidFile := filepath.Join(os.TempDir(), "mainvec_child_pids.txt")
	var pids []byte
	for _, cmd := range pm.ListExecCmdes() {
		if cmd.IsStarted() {
			pids = append(pids, []byte(strconv.Itoa(cmd.Pid())+"\n")...)
		}
	}
	os.WriteFile(pidFile, pids, 0644)
	t.Logf("wrote child PIDs: %s", string(pids))

	select {}
}

// ── Windows process helpers ──────────────────────────────────────────────────

// processAliveWin checks if a process is running on Windows using
// OpenProcess + GetExitCodeProcess. STILL_ACTIVE (259) means running.
func processAliveWin(pid int) bool {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	h, _, _ := k32.NewProc("OpenProcess").Call(0x1000, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer k32.NewProc("CloseHandle").Call(h)
	var code uint32
	k32.NewProc("GetExitCodeProcess").Call(h, uintptr(unsafe.Pointer(&code)))
	return code == 259
}

// killProcessWin terminates a process by PID.
func killProcessWin(pid int) {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	h, _, _ := k32.NewProc("OpenProcess").Call(1, 0, uintptr(pid))
	if h != 0 {
		k32.NewProc("TerminateProcess").Call(h, 1)
		k32.NewProc("CloseHandle").Call(h)
	}
}

// parseWindowsPIDs extracts integer PIDs from newline-separated text.
func parseWindowsPIDs(s string) []int {
	var pids []int
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
