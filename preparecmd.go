package procman

import "os/exec"

// prepareChildCmd applies platform-specific SysProcAttr to the command before
// Start. On Unix the child is placed in its own process group (Setpgid) so a
// negative-pid kill reaches it and its descendants; on Windows the Job Object
// (T8) provides group containment.
func prepareChildCmd(cmd *exec.Cmd) {
	prepareChildCmdPlatform(cmd)
}