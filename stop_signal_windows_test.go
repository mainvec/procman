//go:build windows

package procman_test

import (
	"errors"
	"syscall"
	"testing"

	procman "github.com/mainvec/procman"
)

func TestStopSignalIsUnsupportedOnWindows(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	_, err := pm.NewExecCmd("cmd", nil, procman.WithStopSignal(syscall.SIGINT))
	if !errors.Is(err, procman.ErrStopSignalUnsupported) {
		t.Fatalf("WithStopSignal = %v, want ErrStopSignalUnsupported", err)
	}
}
