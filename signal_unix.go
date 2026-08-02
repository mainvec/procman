//go:build unix

package procman

import (
	"os"
	"os/signal"
	"strings"
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

// isAlreadyGone reports whether err indicates the process is already gone
// (ESRCH on Unix), so callers can avoid treating a reaped process as an error.
func isAlreadyGone(err error) bool {
	if err == nil {
		return false
	}
	if errno, ok := err.(syscall.Errno); ok && errno == syscall.ESRCH {
		return true
	}
	// os.Process.Signal wraps the errno; check the string as a fallback.
	return strings.Contains(err.Error(), "process already finished") ||
		strings.Contains(err.Error(), "no such process")
}

// signalName returns the canonical signal name ("SIGKILL", "SIGSEGV", ...) if
// the process was terminated by a signal, or "" otherwise. The raw
// syscall.Signal.String() yields lowercase forms like "killed"; we normalise
// to the conventional uppercase SIG-prefixed name via os/signal.
func signalName(ps *os.ProcessState) string {
	if ps == nil {
		return ""
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok {
		return ""
	}
	if !ws.Signaled() {
		return ""
	}
	sig := ws.Signal()
	// signal SIGTERM.String() -> "terminated"; use the documented name.
	name := signalNameFor(sig)
	if name != "" {
		return name
	}
	return sig.String()
}

// signalNameFor maps a syscall.Signal to its canonical uppercase "SIG*" name.
// Go's syscall.Signal.String() returns lowercase prose ("killed",
// "terminated", "segmentation fault"), but callers and the plan expect the
// conventional "SIGKILL" form.
func signalNameFor(sig syscall.Signal) string {
	names := map[syscall.Signal]string{
		syscall.SIGHUP:  "SIGHUP",
		syscall.SIGINT:  "SIGINT",
		syscall.SIGQUIT: "SIGQUIT",
		syscall.SIGILL:  "SIGILL",
		syscall.SIGTRAP: "SIGTRAP",
		syscall.SIGABRT: "SIGABRT",
		syscall.SIGBUS:  "SIGBUS",
		syscall.SIGFPE:  "SIGFPE",
		syscall.SIGKILL: "SIGKILL",
		syscall.SIGUSR1: "SIGUSR1",
		syscall.SIGSEGV: "SIGSEGV",
		syscall.SIGUSR2: "SIGUSR2",
		syscall.SIGPIPE: "SIGPIPE",
		syscall.SIGALRM: "SIGALRM",
		syscall.SIGTERM: "SIGTERM",
		syscall.SIGCHLD: "SIGCHLD",
		syscall.SIGCONT: "SIGCONT",
		syscall.SIGSTOP: "SIGSTOP",
		syscall.SIGTSTP: "SIGTSTP",
		syscall.SIGTTIN: "SIGTTIN",
		syscall.SIGTTOU: "SIGTTOU",
	}
	if name, ok := names[sig]; ok {
		return name
	}
	return ""
}

// signalSignal is referenced to ensure the os/signal import is used for
// canonical naming in later tasks (e.g. Notify-based ignore).
var _ = signal.Notify