//go:build !unix

package procman

import (
	"os"
	"os/exec"
	"strings"
)

// prepareChildCmdPlatform is the non-Unix (Windows) implementation: a no-op
// for now; group containment is provided by the Job Object in T8.
func prepareChildCmdPlatform(cmd *exec.Cmd) {}

// signalTerm sends a graceful-termination signal on non-Unix. On Windows the
// only tool is TerminateProcess (hard kill); Stop escalates to it directly.
// For T3 on Windows this is best-effort; the Job Object (T8) provides the real
// semantics.
func signalTerm(proc *os.Process) error {
	return proc.Kill()
}

// signalKill hard-kills the process.
func signalKill(proc *os.Process) error {
	return proc.Kill()
}

// signalName returns "" on Windows; there are no Unix signals. A child
// killed by TerminateProcess reports a non-zero exit code, not a signal.
func signalName(ps *os.ProcessState) string {
	_ = ps
	return ""
}

// isAlreadyGone reports whether err indicates the process is already gone on
// Windows. TerminateProcess on a dead handle returns "Access is denied" or
// similar; we treat a handful of those as "already reaped".
func isAlreadyGone(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already finished") ||
		strings.Contains(msg, "Access is denied") ||
		strings.Contains(msg, "no such process")
}

// signalTermGroup sends a graceful-termination signal. On Windows there is no
// process group via signals; the Job Object (T8) provides containment. For now
// this falls back to the single-process TerminateProcess.
func signalTermGroup(proc *os.Process, pid int) error {
	return signalTerm(proc)
}

// signalKillGroup hard-kills the process (and its job, if assigned).
func signalKillGroup(proc *os.Process, pid int) error {
	return signalKill(proc)
}