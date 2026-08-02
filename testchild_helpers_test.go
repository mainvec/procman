package procman_test

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// itoa is strconv.Itoa to avoid the import in the test files above.
func itoa(i int) string { return strconv.Itoa(i) }

// runtimeGOOS exposes the current GOOS for Unix-only test gating without each
// test file importing runtime.
var runtimeGOOS = runtime.GOOS

// testChildExe returns the test binary path and Env for the supervisor tests.
func testChildExe(t *testing.T) string {
	t.Helper()
	exe, err := procman.TestChildExe()
	if err != nil {
		t.Fatalf("TestChildExe: %v", err)
	}
	return exe
}

// childArgs is a thin wrapper around procman.TestChildArgs for readability.
func childArgs(exitCode int, delay time.Duration, extra ...string) []string {
	return procman.TestChildArgs(exitCode, delay, extra...)
}

// childEnv returns the environment entries to activate the test child.
func childEnv() []string { return procman.TestChildEnv() }

// sigTERM returns SIGTERM on Unix and is unused on Windows (tests skip).
func sigTERM() os.Signal {
	if runtime.GOOS == "windows" {
		return syscall.SIGTERM // not delivered, but keeps the type
	}
	return syscall.SIGTERM
}

// processAlive reports whether a PID is currently alive (and not a zombie).
// Best-effort. A zombie still answers signal 0, so on Linux we read /proc to
// distinguish a real process from an unreaped zombie.
func processAlive(pid int) bool {
	if runtimeGOOS == "windows" {
		// On Windows, use OpenProcess with a query right: a nil handle/error
		// means the process is gone.
		return processAliveWindows(pid)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// signal 0 == existence check on Unix
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	// On Linux, /proc/<pid>/stat's third field is the state; 'Z' is a zombie
	// (dead, awaiting reaping). Treat zombies as not alive.
	if runtimeGOOS == "linux" {
		if state := linuxProcState(pid); state == "Z" {
			return false
		}
	}
	return true
}

// killByPID sends SIGKILL to a PID, best-effort, ignoring "already gone".
func killByPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	if runtime.GOOS == "windows" {
		// Best-effort: not exercised on Windows in this suite.
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		return nil
	}
	return nil
}

// syscallKillGroup sends a signal to the negative process group (Unix only).
// Returns nil if any member is alive, an error otherwise. On Windows this is
// a no-op (process groups are not signal-based).
func syscallKillGroup(pgid, sig int) error {
	if runtime.GOOS == "windows" {
		return os.ErrNotExist
	}
	return syscallKillGroupUnix(pgid, sig)
}

// parsePID extracts the integer after "pid=" in a grandchild announcement line.
func parsePID(line string) int {
	idx := strings.Index(line, "pid=")
	if idx < 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[idx+4:]))
	if err != nil {
		return 0
	}
	return n
}

// waitFor polls cond every 10ms until it returns true or the timeout elapses.
func waitFor(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cond() {
		return nil
	}
	return errors.New("waitFor: condition never became true")
}