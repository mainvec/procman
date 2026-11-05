package procman_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// startLeavingDescendant starts a command whose direct child leaves a sleeping
// descendant behind and exits at once, and waits for that exit.
func startLeavingDescendant(t *testing.T, pm *procman.Procman, sleepSeconds int, opts ...procman.ExecCmdOption) *procman.ExecCmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	cmd, err := pm.NewExecCmd(self, procman.Args(testSpawnMode, strconv.Itoa(sleepSeconds), pidFile), opts...)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-cmd.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("direct child did not exit")
	}
	if code := cmd.ExitCode(); code != 0 {
		t.Fatalf("direct child exited with %d; it could not start the descendant", code)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("descendant PID file: %v", err)
	}
	t.Cleanup(func() { killPIDFile(pidFile) })
	return cmd
}

func killPIDFile(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
	}
}

func awaitTreeDone(t *testing.T, cmd *procman.ExecCmd, within time.Duration) {
	t.Helper()
	select {
	case <-cmd.TreeDone():
	case <-time.After(within):
		t.Fatalf("TreeDone did not close within %v", within)
	}
}

func assertTreeTracked(t *testing.T, cmd *procman.ExecCmd) {
	t.Helper()
	select {
	case <-cmd.TreeDone():
		t.Fatal("TreeDone closed while a descendant is still running")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestTreeDoneWaitsForDescendantsAndStopEndsThem(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startLeavingDescendant(t, pm, 30,
		procman.WithProcessTreeTermination(), procman.WithGracePeriod(5*time.Second))
	assertTreeTracked(t, cmd)

	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitTreeDone(t, cmd, time.Second)
	if cmd.StopRequested() {
		t.Error("StopRequested() is true, but the command itself exited on its own")
	}
}

func TestStopWithZeroGraceEndsTrackedDescendants(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startLeavingDescendant(t, pm, 30, procman.WithProcessTreeTermination())
	assertTreeTracked(t, cmd)

	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitTreeDone(t, cmd, 5*time.Second)
}

func TestKillAllKillsTrackedDescendants(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startLeavingDescendant(t, pm, 30, procman.WithProcessTreeTermination())
	assertTreeTracked(t, cmd)

	pm.KillAll()
	awaitTreeDone(t, cmd, 5*time.Second)
}

func TestShutdownEndsTrackedDescendants(t *testing.T) {
	pm := procman.NewProcman()
	cmd := startLeavingDescendant(t, pm, 30,
		procman.WithProcessTreeTermination(), procman.WithGracePeriod(5*time.Second))
	assertTreeTracked(t, cmd)

	if err := pm.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	awaitTreeDone(t, cmd, time.Second)
}

func TestOrphanCleanupEndsDescendantsWithoutStop(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startLeavingDescendant(t, pm, 30,
		procman.WithProcessTreeTermination(),
		procman.WithOrphanCleanup(),
		procman.WithGracePeriod(5*time.Second),
	)
	awaitTreeDone(t, cmd, 5*time.Second)
	if cmd.StopRequested() {
		t.Error("StopRequested() is true for a command that exited on its own")
	}
}

func TestTreeDoneFollowsDoneWithoutProcessTreeTermination(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startLeavingDescendant(t, pm, 2)
	awaitTreeDone(t, cmd, time.Second)
}

func TestOrphanCleanupRequiresProcessTreeTermination(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	name, args := testExitCommand(t, 0)
	if _, err := pm.NewExecCmd(name, args, procman.WithOrphanCleanup()); err == nil {
		t.Fatal("expected WithOrphanCleanup without WithProcessTreeTermination to fail")
	}
}
