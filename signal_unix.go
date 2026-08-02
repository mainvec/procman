//go:build unix

package procman

import (
	"os"
	"syscall"
)

// signalTerm sends SIGTERM to the process.
func signalTerm(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}

// signalKill sends SIGKILL to the process.
func signalKill(proc *os.Process) error {
	return proc.Signal(syscall.SIGKILL)
}

// signalName returns the signal name that terminated the process, or "".
func signalName(ps *os.ProcessState) string {
	if ps == nil {
		return ""
	}
	if !ps.Exited() {
		// still running; not a signal death
		return ""
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return ws.Signal().String()
		}
	}
	return ""
}