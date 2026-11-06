package procman_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

func isRegistered(pm *procman.Procman, cmd *procman.ExecCmd) bool {
	_, ok := pm.GetExecCmd(cmd.ID())
	return ok
}

func TestRemoveUnknownID(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	if err := pm.Remove(procman.NewID()); !errors.Is(err, procman.ErrExecCmdNotFound) {
		t.Fatalf("Remove(unknown) = %v, want ErrExecCmdNotFound", err)
	}
}

func TestRemoveUnstartedCommandPreventsStart(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	name, args := testSleepCommand(t, 30)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := pm.Remove(cmd.ID()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if isRegistered(pm, cmd) || len(pm.ListExecCmdes()) != 0 {
		t.Fatal("removed command is still registered")
	}
	if err := cmd.Start(); !errors.Is(err, procman.ErrExecCmdRemoved) {
		t.Fatalf("Start after Remove = %v, want ErrExecCmdRemoved", err)
	}
	if cmd.IsStarted() {
		t.Fatal("removed command started")
	}
	if err := pm.Remove(cmd.ID()); !errors.Is(err, procman.ErrExecCmdNotFound) {
		t.Fatalf("second Remove = %v, want ErrExecCmdNotFound", err)
	}
}

func TestRemoveRefusesRunningCommandUntilItFinishes(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	name, args := testSleepCommand(t, 30)
	cmd, err := pm.NewExecCmd(name, args, procman.WithGracePeriod(2*time.Second))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := pm.Remove(cmd.ID()); !errors.Is(err, procman.ErrExecCmdActive) {
		t.Fatalf("Remove(running) = %v, want ErrExecCmdActive", err)
	}
	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitTreeDone(t, cmd, 5*time.Second)
	if err := pm.Remove(cmd.ID()); err != nil {
		t.Fatalf("Remove(finished): %v", err)
	}
	if isRegistered(pm, cmd) {
		t.Fatal("removed command is still registered")
	}
	// The handle stays usable.
	if !cmd.IsExited() {
		t.Fatal("removed command no longer reports its exit")
	}
	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop on removed finished command: %v", err)
	}
}

func TestRemoveRefusesTrackedTree(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startLeavingDescendant(t, pm, 30,
		procman.WithProcessTreeTermination(), procman.WithGracePeriod(2*time.Second))
	if err := pm.Remove(cmd.ID()); !errors.Is(err, procman.ErrExecCmdActive) {
		t.Fatalf("Remove(tracked tree) = %v, want ErrExecCmdActive", err)
	}
	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitTreeDone(t, cmd, 5*time.Second)
	if err := pm.Remove(cmd.ID()); err != nil {
		t.Fatalf("Remove(finished): %v", err)
	}
}

func TestAutoRemoveRemovesFinishedCommandBeforeTreeDone(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	name, args := testExitCommand(t, 0)
	var registeredInOnExit bool
	cmd, err := pm.NewExecCmd(name, args,
		procman.WithAutoRemove(),
		procman.WithOnExit(func(c *procman.ExecCmd, _ error) {
			_, registeredInOnExit = pm.GetExecCmd(c.ID())
		}))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitTreeDone(t, cmd, 5*time.Second)
	if !registeredInOnExit {
		t.Error("command was removed before its WithOnExit callback ran")
	}
	if isRegistered(pm, cmd) {
		t.Fatal("auto-removed command is still registered once TreeDone has closed")
	}
	if err := pm.Remove(cmd.ID()); !errors.Is(err, procman.ErrExecCmdNotFound) {
		t.Fatalf("Remove after auto-removal = %v, want ErrExecCmdNotFound", err)
	}
}

func TestAutoRemoveKeepsCommandWhileTreeIsTracked(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })
	cmd := startLeavingDescendant(t, pm, 30,
		procman.WithProcessTreeTermination(), procman.WithAutoRemove(),
		procman.WithGracePeriod(2*time.Second))
	assertTreeTracked(t, cmd)
	if !isRegistered(pm, cmd) {
		t.Fatal("command was removed while its tree is still tracked")
	}
	if err := pm.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	awaitTreeDone(t, cmd, 5*time.Second)
	if isRegistered(pm, cmd) {
		t.Fatal("auto-removed command is still registered once TreeDone has closed")
	}
}

func TestAutoRemoveAsManagerDefault(t *testing.T) {
	pm, err := procman.NewProcmanWithOptions(procman.WithDefaultExecCmdOptions(procman.WithAutoRemove()))
	if err != nil {
		t.Fatalf("NewProcmanWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = pm.Shutdown() })
	name, args := testExitCommand(t, 3)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitTreeDone(t, cmd, 5*time.Second)
	if isRegistered(pm, cmd) {
		t.Fatal("command is still registered")
	}
	if code := cmd.ExitCode(); code != 3 {
		t.Fatalf("ExitCode after removal = %d, want 3", code)
	}
}

// Exercises Remove, auto-removal, StopAll and Shutdown against each other; run
// with -race.
func TestRemoveRacesWithShutdown(t *testing.T) {
	pm := procman.NewProcman()
	sleepName, sleepArgs := testSleepCommand(t, 30)
	exitName, exitArgs := testExitCommand(t, 0)
	var cmds []*procman.ExecCmd
	for i := 0; i < 8; i++ {
		name, args, opts := exitName, exitArgs, []procman.ExecCmdOption{procman.WithAutoRemove()}
		if i%2 == 0 {
			name, args, opts = sleepName, sleepArgs, []procman.ExecCmdOption{procman.WithGracePeriod(2 * time.Second)}
		}
		cmd, err := pm.NewExecCmd(name, args, opts...)
		if err != nil {
			t.Fatalf("NewExecCmd: %v", err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		cmds = append(cmds, cmd)
	}
	var wg sync.WaitGroup
	for _, cmd := range cmds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				err := pm.Remove(cmd.ID())
				if err == nil || errors.Is(err, procman.ErrExecCmdNotFound) {
					return
				}
				if !errors.Is(err, procman.ErrExecCmdActive) {
					t.Errorf("Remove: %v", err)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	if err := pm.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	wg.Wait()
	for _, cmd := range cmds {
		awaitTreeDone(t, cmd, 5*time.Second)
	}
	if n := len(pm.ListExecCmdes()); n != 0 {
		t.Fatalf("%d commands still registered", n)
	}
}
