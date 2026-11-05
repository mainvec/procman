//go:build unix

package procman

import (
	"errors"
	"os"
	"syscall"
)

type platformState struct{}

func prepareExecCmd(child *ExecCmd) error {
	if !child.processTreeTermination && !child.parentDeathCleanup {
		return nil
	}
	if child.cmd.SysProcAttr == nil {
		child.cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if child.processTreeTermination {
		child.cmd.SysProcAttr.Setpgid = true
	}
	if child.parentDeathCleanup {
		return configureParentDeathSignal(child)
	}
	return nil
}

func finishExecCmdStart(child *ExecCmd) error {
	return nil
}

func cleanupExecCmd(child *ExecCmd) {}

func gracefulSignal(child *ExecCmd) error {
	if child.processTreeTermination {
		return signalProcessGroup(child.cmd.Process.Pid, syscall.SIGTERM)
	}
	return ignoreProcessDone(child.cmd.Process.Signal(syscall.SIGTERM))
}

func forceKill(child *ExecCmd) error {
	if child.processTreeTermination {
		return signalProcessGroup(child.cmd.Process.Pid, syscall.SIGKILL)
	}
	return ignoreProcessDone(child.cmd.Process.Kill())
}

// ignoreProcessDone treats a child the reaper has already collected as stopped.
// Stop checks status under the lock and signals after releasing it, so the
// reaper can win that race; the outcome the caller wanted has happened.
func ignoreProcessDone(err error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

// gracefulTreeSignal reports that a tree whose direct child has been reaped
// can still be signalled gracefully: its process group outlives the leader.
const gracefulTreeSignal = true

// treeAlive reports whether any signallable process remains in the command's
// process group. It is consulted when the group leader is reaped, and from
// then on only while the group is tracked, until it is first seen empty. While
// a member remains, POSIX does not reuse the group's ID. A group whose only
// members refuse our signals (EPERM) counts as empty: nothing could be done
// to them anyway.
func treeAlive(child *ExecCmd) bool {
	if !child.processTreeTermination {
		return false
	}
	return syscall.Kill(-child.cmd.Process.Pid, 0) == nil
}

// signalTree signals the command's process group after its leader has been
// reaped. Callers ensure the group has just been seen populated.
func signalTree(child *ExecCmd, graceful bool) error {
	if graceful {
		return signalProcessGroup(child.cmd.Process.Pid, syscall.SIGTERM)
	}
	return signalProcessGroup(child.cmd.Process.Pid, syscall.SIGKILL)
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func processIsAlive(proc *os.Process) bool {
	return proc.Signal(syscall.Signal(0)) == nil
}
