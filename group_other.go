//go:build !unix

package procman

import "os"

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