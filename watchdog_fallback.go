//go:build unix

package procman

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// watchdogFallbackEnv is the env var that triggers the re-exec fallback: the
// host binary re-executes itself with this set, and the init() hook runs the
// watchdog loop in Go (never returning to main). Selected automatically when
// /bin/sh is absent.
const watchdogFallbackEnv = "PROCMAN_WATCHDOG"

// runWatchdogFallback is the Go implementation of the /bin/sh sidecar script.
// It reads the pgid from fd 3 (the sentinel pipe), then either exits 0
// (parent died before arming, or parent stood us down), or kills the target
// process group: SIGTERM -> grace -> SIGKILL.
//
// This is selected automatically when the shell is absent, and via
// RunWatchdogAndExit (which a host can call as the first statement of main).
func runWatchdogFallback() {
	// fd 3 is the read end of the sentinel pipe, inherited via ExtraFiles.
	fd := os.NewFile(3, "procman-watchdog")
	if fd == nil {
		// No fd 3: nothing to do (e.g. PROCMAN_WATCHDOG set with no pipe).
		os.Exit(0)
	}

	// Read the first line: the pgid. EOF here means the parent died before
	// arming — nothing exists to kill, exit 0.
	line, err := readLine(fd)
	if err != nil {
		// EOF or read error before arming: nothing to kill.
		os.Exit(0)
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pgid <= 0 {
		// Invalid pgid: nothing safe to kill; exit cleanly.
		os.Exit(0)
	}

	// Read the second line: the stand-down. A line means the target exited
	// normally and the parent stood us down — exit 0. EOF means the parent
	// died while the target runs — kill the group.
	_, err = readLine(fd)
	if err == nil {
		// Got a stand-down line: exit cleanly without killing.
		os.Exit(0)
	}

	// Parent died while the target runs: SIGTERM -> grace -> SIGKILL on the
	// whole process group. ESRCH (group already gone) is not an error.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(5 * time.Second)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	os.Exit(0)
}

// readLine reads a single newline-terminated line from f, without the newline.
// Returns an error on EOF with no data.
func readLine(f *os.File) (string, error) {
	var buf [1]byte
	var sb strings.Builder
	for {
		n, err := f.Read(buf[:])
		if n > 0 {
			if buf[0] == '\n' {
				return sb.String(), nil
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			if sb.Len() > 0 {
				return sb.String(), nil
			}
			return "", err
		}
	}
}

// RunWatchdogAndExit runs the Go watchdog loop if the process was re-executed
// as a fallback watchdog (PROCMAN_WATCHDOG=1). It never returns in that case.
// A host with no /bin/sh may call it as the first statement of main instead of
// relying on the init() hook. If the env var is unset, it returns immediately.
func RunWatchdogAndExit() {
	if os.Getenv(watchdogFallbackEnv) == "" {
		return
	}
	runWatchdogFallback()
}

func init() {
	// The init() hook intercepts a re-executed fallback watchdog. It runs
	// before main, which is the cost of the fallback on systems with no
	// /bin/sh (the host's package-level init runs once per spawn). A host that
	// wants to avoid this calls RunWatchdogAndExit explicitly.
	if os.Getenv(watchdogFallbackEnv) == "" {
		return
	}
	runWatchdogFallback()
}

// spawnWatchdogFallback re-executes the host binary with PROCMAN_WATCHDOG=1
// and the sentinel pipe on fd 3, used when /bin/sh is absent (WatchdogAuto on
// a system with no shell, or ShellPath pointed at a nonexistent file).
func (s *Supervisor) spawnWatchdogFallback(spec Spec) (*os.Process, *os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("procman: fallback watchdog pipe: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, nil, fmt.Errorf("procman: fallback watchdog exe: %v", err)
	}
	// Re-exec the host binary; the init() hook intercepts via the env var.
	proc, err := os.StartProcess(exe, []string{exe}, &os.ProcAttr{
		Env: append(os.Environ(), watchdogFallbackEnv+"=1"),
		Files: []*os.File{nil, nil, nil, r}, // fd 3 is the read end
		Sys:   fallbackSysProcAttr(),
	})
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, nil, fmt.Errorf("procman: fallback watchdog start: %v", err)
	}
	_ = r.Close()
	return proc, w, nil
}