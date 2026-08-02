//go:build !unix

package procman

import (
	"os"
)

// On non-Unix (Windows) the parent-death guarantee is provided by the Job
// Object; there is no /bin/sh sidecar and no re-exec fallback. These stubs
// keep the cross-platform startGeneration path compiling.

// watchdog is the unified sidecar handle. On non-Unix it is never created.
type watchdog struct {
	proc *os.Process
	pipe *os.File
	done chan struct{}
}

// spawnWatchdog returns no watchdog on non-Unix.
func (s *Supervisor) spawnWatchdog(spec Spec) (*watchdog, error) {
	return nil, nil
}

// armWatchdog is a no-op on non-Unix (no sidecar).
func armWatchdog(wd *watchdog, pgid int) error { return nil }

// standDownWatchdog is a no-op on non-Unix (no sidecar).
func standDownWatchdog(wd *watchdog) {}

// watchdogEnabled is false on non-Unix; the Job Object provides the guarantee.
func (s *Supervisor) watchdogEnabled() bool { return false }

// RunWatchdogAndExit is a no-op on non-Unix; the Job Object provides the
// parent-death guarantee.
func RunWatchdogAndExit() {}

// watchdogFallbackEnv is unused on non-Unix but kept for API symmetry.
const watchdogFallbackEnv = "PROCMAN_WATCHDOG"