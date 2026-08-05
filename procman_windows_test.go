//go:build windows

// These tests verify procman functionality on Windows using the test binary
// itself as a portable sleep command (--procman-sleep N), since Windows does
// not ship with a "sleep" or "true" executable. The parent-death test spawns
// a child process of the test binary, starts managed grandchildren with
// WithParentDeathCleanup, kills the parent, and verifies that the kernel
// terminates the grandchildren via the kill-on-close Job Object.

package procman_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	procman "github.com/mainvec/procman"
)

// sleepName returns the test binary path and arguments that sleep for the
// given number of seconds. The test binary handles --procman-sleep in init().
func sleepName(secs int) (string, []string) {
	self, _ := os.Executable()
	return self, []string{"--procman-sleep", strconv.Itoa(secs)}
}

// init handles the --procman-sleep mode so the test binary can act as a
// portable sleep command on Windows.
func init() {
	if len(os.Args) > 2 && os.Args[1] == "--procman-sleep" {
		secs, _ := strconv.Atoi(os.Args[2])
		if secs == 0 {
			secs = 60
		}
		time.Sleep(time.Duration(secs) * time.Second)
		os.Exit(0)
	}
}

// TestWindowsNewProcman tests basic start and wait on Windows.
func TestWindowsNewProcman(t *testing.T) {
	pm := procman.NewProcman()
	name, args := sleepName(1)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !cmd.IsExited() {
		t.Fatal("expected IsExited=true")
	}
}

// TestWindowsMultipleExecCmds tests multiple concurrent commands with
// OnStart and OnExit callbacks.
func TestWindowsMultipleExecCmds(t *testing.T) {
	pm := procman.NewProcman()
	var onStart, onExit atomic.Int32
	pm.OnStart = func(cmd *procman.ExecCmd) { onStart.Add(1) }
	pm.OnExit = func(cmd *procman.ExecCmd, err error) { onExit.Add(1) }

	n1, a1 := sleepName(1)
	n2, a2 := sleepName(2)
	c1, _ := pm.NewExecCmd(n1, a1)
	c2, _ := pm.NewExecCmd(n2, a2)
	c1.Start()
	c2.Start()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c1.Wait() }()
	go func() { defer wg.Done(); c2.Wait() }()
	wg.Wait()

	pm.WaitEventLoop()
	if onStart.Load() != 2 {
		t.Fatalf("expected 2 OnStart, got %d", onStart.Load())
	}
	if onExit.Load() != 2 {
		t.Fatalf("expected 2 OnExit, got %d", onExit.Load())
	}
}

// TestWindowsStop tests graceful stop with a grace period.
func TestWindowsStop(t *testing.T) {
	pm := procman.NewProcman()
	var onExit atomic.Int32
	pm.OnExit = func(cmd *procman.ExecCmd, err error) { onExit.Add(1) }

	name, args := sleepName(60)
	cmd, _ := pm.NewExecCmd(name, args, procman.WithGracePeriod(5*time.Second))
	cmd.Start()
	time.Sleep(200 * time.Millisecond)
	if !cmd.IsRunning() {
		t.Fatal("expected running")
	}
	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !cmd.IsExited() {
		t.Fatal("expected exited")
	}
	pm.WaitEventLoop()
	if onExit.Load() != 1 {
		t.Fatalf("expected 1 OnExit, got %d", onExit.Load())
	}
}

// TestWindowsStopAll tests concurrent stop of multiple commands.
func TestWindowsStopAll(t *testing.T) {
	pm := procman.NewProcman()
	for range 2 {
		name, args := sleepName(60)
		cmd, _ := pm.NewExecCmd(name, args, procman.WithGracePeriod(100*time.Millisecond))
		cmd.Start()
	}
	if err := pm.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
}

// TestWindowsShutdown tests that Shutdown stops processes, drains callbacks,
// and rejects new commands.
func TestWindowsShutdown(t *testing.T) {
	pm := procman.NewProcman()
	var onExit atomic.Int32
	pm.OnExit = func(cmd *procman.ExecCmd, err error) { onExit.Add(1) }

	name, args := sleepName(60)
	cmd, _ := pm.NewExecCmd(name, args, procman.WithGracePeriod(100*time.Millisecond))
	cmd.Start()
	if err := pm.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !cmd.IsExited() {
		t.Fatal("expected exited after Shutdown")
	}
	if onExit.Load() != 1 {
		t.Fatalf("expected 1 OnExit, got %d", onExit.Load())
	}
}

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
			procman.Args("--procman-sleep", "120"),
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