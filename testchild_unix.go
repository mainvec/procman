//go:build unix

package procman

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func testChildSetsid() error {
	// Create a new session so this child escapes the supervisor's process
	// group. This models the documented Unix limitation: a setsid child is
	// not reached by kill(-pgid).
	if _, err := syscall.Setsid(); err != nil {
		return err
	}
	return nil
}

func ignoreSIGTERM() {
	// Ignore SIGTERM so Stop must escalate to SIGKILL. Use Notify so the Go
	// runtime does not terminate us; the signal is then dropped.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		for range ch {
			// drop
		}
	}()
}

func prepareGrandchildCmd(cmd *exec.Cmd) {
	// Grandchildren are started directly by the test child, not by the
	// supervisor, so no group attributes are set here. They inherit the
	// child's process group, which is what the group-kill must reach.
}