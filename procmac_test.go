package procman_test

import (
	"testing"

	procman "github.com/mainvec/procmac"
)

func TestNewProcman(t *testing.T) {
	procman := procman.NewProcman()
	if procman == nil {
		t.Fatal("expected a procmac instance")
	}
	child := procman.NewExecCmd("echo", "Hello, World!")
	err := child.Start()
	if err != nil {
		t.Fatal(err)
	}
	err = child.Wait()
	if err != nil {
		t.Fatal(err)
	}
	s := child.GetProcessState()
	if s == nil {
		t.Fatal("expected a process state")
	}
	if !s.Success() {
		t.Fatal("expected process to exit successfully")
	}
}
