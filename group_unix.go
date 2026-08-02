//go:build unix

package procman

import (
	"os"
	"os/exec"
	"syscall"
)

// prepareChildCmdPlatform is the Unix implementation: place the child in its
// own process group.
func prepareChildCmdPlatform(cmd *exec.Cmd) {
	prepareChildCmdUnix(cmd)
}

// newGroupAttrs returns the SysProcAttr that puts the child in its own
// process group (Setpgid). The child becomes a process-group leader, so a
// negative-pid kill reaches it and all its descendants. The pgid equals the
// child's pid.
func newGroupAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// prepareChildCmdUnix sets the process-group attribute on the command. This
// is the Unix half of prepareChildCmd.
func prepareChildCmdUnix(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = newGroupAttrs()
		return
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminateGroup sends SIGTERM to the whole process group identified by pgid.
// ESRCH (group already gone) is not an error.
func terminateGroup(pgid int) error {
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	return nil
}

// killGroup sends SIGKILL to the whole process group identified by pgid.
// ESRCH (group already gone) is not an error.
func killGroup(pgid int) error {
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	return nil
}

// groupProcessFor returns an *os.Process whose Signal targets the group via a
// negative pgid, for compatibility with the signalTerm/signalKill seam used by
// stop. We synthesise it from the pid.
func groupProcessFor(pgid int) (*os.Process, error) {
	return os.FindProcess(-pgid)
}

// assignToGroupHandle assigns the just-started child to its group/container.
// On Unix the group is established by Setpgid in prepareChildCmdPlatform, so
// there is no handle to return — the kernel owns the group.
func assignToGroupHandle(cmd *exec.Cmd) (groupHandle, error) {
	return nil, nil
}

// fallbackSysProcAttr returns the SysProcAttr for the re-executed fallback
// watchdog: its own process group, so the target's group kill does not sweep
// it up.
func fallbackSysProcAttr() *syscall.SysProcAttr {
	return newGroupAttrs()
}

// signalTermGroup sends SIGTERM to the whole process group. On Unix the child
// is its own group leader (Setpgid), so pgid == pid; kill(-pgid, SIGTERM)
// reaches it and all descendants.
func signalTermGroup(proc *os.Process, pgid int) error {
	return terminateGroup(pgid)
}

// signalKillGroup sends SIGKILL to the whole process group.
func signalKillGroup(proc *os.Process, pgid int) error {
	return killGroup(pgid)
}