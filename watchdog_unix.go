//go:build unix

package procman

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// watchdogScript is the fixed /bin/sh sidecar. It blocks on a sentinel pipe
// (fd 3 in the child) and kills the target's process group on parent death.
//
//	read pgid <&3 || exit 0        # parent died before arming; nothing to kill
//	read done <&3 && exit 0        # parent stood us down; target exited
//	kill -TERM -- -$pgid 2>/dev/null
//	sleep $grace
//	kill -KILL -- -$pgid 2>/dev/null
//
// No user-controlled data is interpolated: the pgid arrives over the pipe and
// is validated as an integer before use. Only the grace (a caller-supplied
// duration, converted to whole seconds) is embedded as a numeric literal.
func watchdogScript(graceSec int) string {
	return strings.Join([]string{
		"read pgid <&3 || exit 0",
		"read done <&3 && exit 0",
		"kill -TERM -- -$pgid 2>/dev/null",
		"sleep " + strconv.Itoa(graceSec),
		"kill -KILL -- -$pgid 2>/dev/null",
	}, "\n")
}

// watchdogShell returns the shell path for the sidecar: Options.ShellPath if
// set, else /bin/sh.
func (s *Supervisor) watchdogShell() string {
	if s.opts.ShellPath != "" {
		return s.opts.ShellPath
	}
	return "/bin/sh"
}

// spawnWatchdog starts the /bin/sh sidecar with the sentinel pipe on fd 3.
// It returns the running *exec.Cmd (the watchdog) and the *os.File write-end
// of the sentinel pipe, which the caller must hold until it writes the
// stand-down line and closes it. Spawn ordering is watchdog → target → write
// pgid, so this must be called BEFORE startGeneration launches the target.
//
// The returned write-end is the parent's reference to the pipe; os.File has a
// finalizer that closes the fd, so the caller must keep it reachable until
// stand-down to avoid a spurious watchdog trigger.
func (s *Supervisor) spawnWatchdog(spec Spec) (*exec.Cmd, *os.File, error) {
	// Create the sentinel pipe. The child reads from the read end (fd 3 via
	// ExtraFiles); the parent holds the write end.
	r, w, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("procman: watchdog pipe: %v", err)
	}

	graceSec := int(spec.StopGrace / time.Second)
	if graceSec <= 0 {
		graceSec = 5
	}
	cmd := exec.Command(s.watchdogShell(), "-c", watchdogScript(graceSec))
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	// ExtraFiles[0] becomes fd 3 in the child.
	cmd.ExtraFiles = []*os.File{r}
	// Put the watchdog in its own process group so it is not swept up by the
	// target's group kill.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = newGroupAttrs()
	}

	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, nil, fmt.Errorf("procman: watchdog start: %v", err)
	}
	// The parent does not need the read end; close it so the only read end is
	// in the watchdog. When the parent dies, the write end closes and the
	// watchdog sees EOF.
	_ = r.Close()
	return cmd, w, nil
}

// armWatchdog writes the target's pgid to the sentinel pipe, arming the
// watchdog. After this, EOF on the pipe (parent death) triggers the kill
// sequence. The pgid is validated as a positive integer before writing.
func armWatchdog(w *os.File, pgid int) error {
	if pgid <= 0 {
		return fmt.Errorf("procman: invalid pgid %d", pgid)
	}
	line := strconv.Itoa(pgid) + "\n"
	if _, err := w.WriteString(line); err != nil {
		return fmt.Errorf("procman: arm watchdog: %v", err)
	}
	return nil
}

// standDownWatchdog writes the stand-down line so the watchdog exits cleanly
// without killing the group, then closes the pipe.
func standDownWatchdog(w *os.File) {
	if w == nil {
		return
	}
	_, _ = w.WriteString("done\n")
	_ = w.Close()
}

// watchdogEnabled reports whether the watchdog sidecar is active for this
// platform and Options.
func (s *Supervisor) watchdogEnabled() bool {
	if s.opts.Watchdog == WatchdogOff {
		return false
	}
	// WatchdogAuto: active on Unix (sidecar). Windows uses a Job Object and
	// needs no watchdog. This file is unix-only, so auto == enabled here.
	return true
}