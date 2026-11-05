//go:build linux

package procman

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPrepareExecCmdParentDeathSignal(t *testing.T) {
	terminateChild := &ExecCmd{
		cmd:                exec.Command("sleep", "60"),
		parentDeathCleanup: true,
	}
	if err := prepareExecCmd(terminateChild); err != nil {
		t.Fatalf("prepare Terminate command: %v", err)
	}
	if got := terminateChild.cmd.SysProcAttr.Pdeathsig; got != syscall.SIGKILL {
		t.Fatalf("expected Pdeathsig SIGKILL, got %v", got)
	}

	noneChild := &ExecCmd{
		cmd: exec.Command("sleep", "60"),
	}
	if err := prepareExecCmd(noneChild); err != nil {
		t.Fatalf("prepare None command: %v", err)
	}
	if noneChild.cmd.SysProcAttr != nil && noneChild.cmd.SysProcAttr.Pdeathsig != 0 {
		t.Fatalf("expected no parent-death signal, got %v", noneChild.cmd.SysProcAttr.Pdeathsig)
	}
}

func TestTerminatePolicyKillsDirectChildWhenParentDies(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	parent := exec.Command(testBinary, "-test.run=^TestLinuxParentDeathHelper$")
	parent.Env = append(os.Environ(),
		"PROCMAN_LINUX_PARENT_HELPER=1",
		"PROCMAN_CHILD_PID_FILE="+pidFile,
	)
	if err := parent.Start(); err != nil {
		t.Fatalf("start parent helper: %v", err)
	}

	childPID := waitForLinuxChildPID(t, pidFile)
	defer syscall.Kill(-childPID, syscall.SIGKILL)
	if err := parent.Process.Kill(); err != nil {
		t.Fatalf("kill parent helper: %v", err)
	}
	if err := parent.Wait(); err == nil {
		t.Fatal("expected killed parent helper to exit unsuccessfully")
	}
	waitForLinuxProcessExit(t, childPID)
}

func TestLinuxParentDeathHelper(t *testing.T) {
	if os.Getenv("PROCMAN_LINUX_PARENT_HELPER") != "1" {
		return
	}
	pm := NewProcman()
	cmd, err := pm.NewExecCmd("/bin/sh", Args("-c",
		`echo $$ > "$PROCMAN_CHILD_PID_FILE"; exec sleep 60`),
		WithParentDeathCleanup(),
	)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = cmd.Wait()
}

func waitForLinuxChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
			if parseErr != nil {
				t.Fatalf("parse child PID: %v", parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read child PID: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for child PID")
	return 0
}

func waitForLinuxProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		// ESRCH: the entry was opened while the process was a zombie and read
		// after it was reaped. Either way it has exited.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatalf("read process state: %v", err)
		}
		closeParen := strings.LastIndexByte(string(stat), ')')
		if closeParen >= 0 && len(stat) > closeParen+2 && stat[closeParen+2] == 'Z' {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected child %d to terminate after parent death", pid)
}

// Pdeathsig fires when the thread that forked the child exits, not when the
// process does. A goroutine that locks its thread and returns makes Go
// terminate that thread. A child forked on it would then be killed while the
// procman-owning process is still running.
func TestParentDeathCleanupSurvivesExitOfStartingThread(t *testing.T) {
	pm := NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	// Three, because one locked goroutine may land on the main thread, which Go
	// parks instead of terminating.
	var cmds []*ExecCmd
	for range 3 {
		cmd, err := pm.NewExecCmd("sleep", Args("30"), WithParentDeathCleanup())
		if err != nil {
			t.Fatalf("NewExecCmd: %v", err)
		}
		started := make(chan error, 1)
		go func() {
			runtime.LockOSThread() // never unlocked: the thread ends with the goroutine
			started <- cmd.Start()
		}()
		if err := <-started; err != nil {
			t.Fatalf("Start: %v", err)
		}
		cmds = append(cmds, cmd)
	}

	time.Sleep(500 * time.Millisecond)
	for i, cmd := range cmds {
		if cmd.IsExited() {
			t.Errorf("child %d was killed (exit code %d) when the thread that started it exited",
				i, cmd.ExitCode())
		}
	}
}

func TestLockedThreadReturnsErrorsAndRepanicsOnTheCaller(t *testing.T) {
	var thread lockedThread
	want := errors.New("boom")
	if err := thread.run(func() error { return want }); err != want {
		t.Fatalf("run returned %v, want %v", err, want)
	}

	func() {
		defer func() {
			if r := recover(); r != "panicked" {
				t.Fatalf("recovered %v, want the panic from fn", r)
			}
		}()
		_ = thread.run(func() error { panic("panicked") })
	}()

	// The thread survives a panicking call.
	if err := thread.run(func() error { return nil }); err != nil {
		t.Fatalf("run after panic: %v", err)
	}
}
