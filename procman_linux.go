//go:build linux

package procman

import "syscall"

// SupportsParentDeathCleanup reports whether native parent-death cleanup is
// available on the current platform.
func SupportsParentDeathCleanup() bool {
	return true
}

func configureParentDeathSignal(child *ExecCmd) error {
	child.cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	return nil
}
