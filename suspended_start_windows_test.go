//go:build windows

package procman

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

	"golang.org/x/sys/windows"
)

const spawnNowMode = "--procman-spawn-now"

func init() {
	if len(os.Args) < 3 || os.Args[1] != spawnNowMode {
		return
	}
	// Start a grandchild as the very first thing, then stay alive. The sleep
	// mode is handled by the external test package's helper.
	self, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	grandchild := exec.Command(self, "--procman-sleep", "30")
	if err := grandchild.Start(); err != nil {
		os.Exit(3)
	}
	_ = os.WriteFile(os.Args[2], []byte(strconv.Itoa(grandchild.Process.Pid)), 0o600)
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func inJob(t *testing.T, c *ExecCmd, pid int) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("OpenProcess(%d): %v", pid, err)
	}
	defer windows.CloseHandle(h)
	c.platform.mu.Lock()
	job := c.platform.job
	c.platform.mu.Unlock()
	var result int32
	if r, _, err := procIsProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&result))); r == 0 {
		t.Fatalf("IsProcessInJob(%d): %v", pid, err)
	}
	return result != 0
}

func readPIDFile(path string, within time.Duration) (int, bool) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				return pid, true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, false
}

func killPID(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// A child that starts a process before procman has assigned it to its Job
// Object would leave that process outside the job. The hook widens the window
// to 200ms; created suspended, the child cannot use it.
func TestJobContainsProcessStartedBeforeAssignment(t *testing.T) {
	afterStartHook = func() { time.Sleep(200 * time.Millisecond) }
	t.Cleanup(func() { afterStartHook = nil })

	pm := NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	c, err := pm.NewExecCmd(self, Args(spawnNowMode, pidFile),
		WithProcessTreeTermination(), WithGracePeriod(time.Second))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	grandchild, ok := readPIDFile(pidFile, 10*time.Second)
	if !ok {
		t.Fatal("grandchild never started")
	}
	t.Cleanup(func() { killPID(grandchild) })
	if !inJob(t, c, c.Pid()) {
		t.Error("child is not in its Job Object")
	}
	if !inJob(t, c, grandchild) {
		t.Fatal("grandchild started before assignment escaped the Job Object")
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-c.TreeDone():
	case <-time.After(10 * time.Second):
		t.Fatal("tree, grandchild included, not ended by Stop")
	}
}

// A caller that creates the child suspended keeps control of resuming it.
func TestCallerSuspendedChildIsNotResumed(t *testing.T) {
	pm := NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := exec.Command(self, spawnNowMode, pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	c, err := pm.NewExecCmdFromCmd(cmd, WithProcessTreeTermination(), WithGracePeriod(time.Second))
	if err != nil {
		t.Fatalf("NewExecCmdFromCmd: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := readPIDFile(pidFile, 500*time.Millisecond); ok {
		t.Fatal("procman resumed a child the caller created suspended")
	}
	if err := resumeProcess(c.Pid()); err != nil {
		t.Fatalf("resumeProcess: %v", err)
	}
	grandchild, ok := readPIDFile(pidFile, 10*time.Second)
	if !ok {
		t.Fatal("child did not run once resumed")
	}
	t.Cleanup(func() { killPID(grandchild) })
	if !inJob(t, c, grandchild) {
		t.Error("grandchild escaped the Job Object")
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// Without a Job Object the child is created as before, not suspended.
func TestChildWithoutJobObjectIsNotSuspended(t *testing.T) {
	pm := NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	c, err := pm.NewExecCmd(self, Args(spawnNowMode, pidFile))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.cmd.Process.Kill() })
	if c.cmd.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED != 0 {
		t.Fatal("child without a Job Object was created suspended")
	}
	grandchild, ok := readPIDFile(pidFile, 10*time.Second)
	if !ok {
		t.Fatal("child did not run")
	}
	killPID(grandchild)
}

func TestResumeProcessFailsWithoutThreads(t *testing.T) {
	// Windows process IDs are multiples of four, so 3 names no process.
	if err := resumeProcess(3); err == nil {
		t.Fatal("resumeProcess succeeded for a process that does not exist")
	}
}
