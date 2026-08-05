//go:build windows

package procman

import (
	"os/exec"
	"testing"
)

func TestPrepareExecCmdJobObjectPolicy(t *testing.T) {
	terminateChild := &ExecCmd{
		cmd:                    exec.Command("cmd.exe"),
		processTreeTermination: true,
	}
	if err := prepareExecCmd(terminateChild); err != nil {
		t.Fatalf("prepare Terminate command: %v", err)
	}
	if terminateChild.platform.job == 0 {
		t.Fatal("expected Terminate policy to create a Job Object")
	}
	cleanupExecCmd(terminateChild)

	noneChild := &ExecCmd{
		cmd: exec.Command("cmd.exe"),
	}
	if err := prepareExecCmd(noneChild); err != nil {
		t.Fatalf("prepare None command: %v", err)
	}
	if noneChild.platform.job != 0 {
		t.Fatal("expected None policy not to create a Job Object")
	}
}
