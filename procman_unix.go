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
	return child.cmd.Process.Signal(syscall.SIGTERM)
}

func forceKill(child *ExecCmd) error {
	if child.processTreeTermination {
		return signalProcessGroup(child.cmd.Process.Pid, syscall.SIGKILL)
	}
	return child.cmd.Process.Kill()
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
