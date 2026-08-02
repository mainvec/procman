//go:build unix

package procman

import "os/exec"

func prepareExecCmd(child *ExecCmd) error {
	// Implement any necessary preparation steps for the child process here.
	// This could include setting environment variables, redirecting output, etc.
	return nil
}

func prepareChildCmd(cmd *exec.Cmd) {
	// Implement any necessary preparation steps for the exec.Cmd here.
	// This could include setting environment variables, redirecting output, etc.
}
