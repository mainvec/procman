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
//	kill -TERM -"$pgid" 2>/dev/null
//	sleep $grace
//	kill -KILL -"$pgid" 2>/dev/null
//
// No user-controlled data is interpolated: the pgid arrives over the pipe and
// is validated as an integer before use. Only the grace (a caller-supplied
// duration, converted to whole seconds) is embedded as a numeric literal.
// The negative-pid form without "--" is portable across dash, bash and ash;
// the "--" delimiter is not universally accepted by dash's kill builtin.
func watchdogScript(graceSec int) string {
	return strings.Join([]string{
		"read pgid <&3 || exit 0",
		"read done <&3 && exit 0",
		`kill -TERM -"$pgid" 2>/dev/null`,
		"sleep " + strconv.Itoa(graceSec),
		`kill -KILL -"$pgid" 2>/dev/null`,
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

// watchdog is the unified sidecar handle, whether backed by /bin/sh or the
// re-exec fallback. The parent holds the sentinel write-end and stands it
// down on reap; it owns the process so it can be reaped (no zombie).
type watchdog struct {
	proc *os.Process
	pipe *os.File
	// done is closed when the watchdog process exits.
	done chan struct{}
}

// spawnWatchdog starts the /bin/sh sidecar with the sentinel pipe on fd 3,
// or — when the shell is absent — the re-exec fallback. It returns the
// watchdog handle whose pipe the caller must keep reachable until stand-down.
// Spawn ordering is watchdog → target → write pgid, so this is called BEFORE
// startGeneration launches the target.
func (s *Supervisor) spawnWatchdog(spec Spec) (*watchdog, error) {
	shell := s.watchdogShell()
	if shellAvailable(shell) {
		return s.spawnShellWatchdog(spec, shell)
	}
	// Shell absent: use the re-exec fallback.
	return s.spawnFallbackWatchdog(spec)
}

// shellAvailable reports whether the shell path exists and is executable.
func shellAvailable(shell string) bool {
	if shell == "" {
		return false
	}
	info, err := os.Stat(shell)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// spawnShellWatchdog starts the /bin/sh sidecar.
func (s *Supervisor) spawnShellWatchdog(spec Spec, shell string) (*watchdog, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("procman: watchdog pipe: %v", err)
	}

	graceSec := int(spec.StopGrace / time.Second)
	if graceSec <= 0 {
		graceSec = 5
	}
	cmd := exec.Command(shell, "-c", watchdogScript(graceSec))
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.ExtraFiles = []*os.File{r} // becomes fd 3 in the child
	// Put the watchdog in its own process group so it is not swept up by the
	// target's group kill.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = newGroupAttrs()
	}

	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, fmt.Errorf("procman: watchdog start: %v", err)
	}
	_ = r.Close() // the only read end is now in the watchdog
	wd := &watchdog{proc: cmd.Process, pipe: w, done: make(chan struct{})}
	go func() {
		_, _ = cmd.Process.Wait()
		close(wd.done)
	}()
	return wd, nil
}

// spawnFallbackWatchdog starts the re-executed host-binary fallback when no
// shell is available.
func (s *Supervisor) spawnFallbackWatchdog(spec Spec) (*watchdog, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("procman: fallback watchdog pipe: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, fmt.Errorf("procman: fallback watchdog exe: %v", err)
	}
	proc, err := os.StartProcess(exe, []string{exe}, &os.ProcAttr{
		Env:   append(os.Environ(), watchdogFallbackEnv+"=1"),
		Files: []*os.File{nil, nil, nil, r}, // fd 3 is the read end
		Sys:   fallbackSysProcAttr(),
	})
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, fmt.Errorf("procman: fallback watchdog start: %v", err)
	}
	_ = r.Close()
	wd := &watchdog{proc: proc, pipe: w, done: make(chan struct{})}
	go func() {
		_, _ = proc.Wait()
		close(wd.done)
	}()
	return wd, nil
}

// armWatchdog writes the target's pgid to the sentinel pipe, arming the
// watchdog. After this, EOF on the pipe (parent death) triggers the kill
// sequence. The pgid is validated as a positive integer before writing.
func armWatchdog(wd *watchdog, pgid int) error {
	if wd == nil || wd.pipe == nil {
		return nil
	}
	if pgid <= 0 {
		return fmt.Errorf("procman: invalid pgid %d", pgid)
	}
	line := strconv.Itoa(pgid) + "\n"
	if _, err := wd.pipe.WriteString(line); err != nil {
		return fmt.Errorf("procman: arm watchdog: %v", err)
	}
	return nil
}

// standDownWatchdog writes the stand-down line so the watchdog exits cleanly
// without killing the group, then closes the pipe. Idempotent.
func standDownWatchdog(wd *watchdog) {
	if wd == nil || wd.pipe == nil {
		return
	}
	_, _ = wd.pipe.WriteString("done\n")
	_ = wd.pipe.Close()
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