//go:build !unix

package procman

import (
	"os"
	"os/exec"
)

// On non-Unix (Windows) the parent-death guarantee is provided by the Job
// Object; there is no /bin/sh sidecar. These stubs keep the cross-platform
// startGeneration path compiling.

// spawnWatchdog returns no watchdog on non-Unix.
func (s *Supervisor) spawnWatchdog(spec Spec) (*exec.Cmd, *os.File, error) {
	return nil, nil, nil
}

// armWatchdog is a no-op on non-Unix (no sidecar).
func armWatchdog(w *os.File, pgid int) error { return nil }

// standDownWatchdog is a no-op on non-Unix (no sidecar).
func standDownWatchdog(w *os.File) {}

// watchdogEnabled is false on non-Unix; the Job Object provides the guarantee.
func (s *Supervisor) watchdogEnabled() bool { return false }