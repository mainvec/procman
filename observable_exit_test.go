package procman_test

import (
	"sync"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// awaitDone fails the test rather than hanging it when a command is never
// reaped.
func awaitDone(t *testing.T, cmd *procman.ExecCmd) {
	t.Helper()
	select {
	case <-cmd.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the command to be reaped")
	}
}

func TestDoneClosesOnceTheCommandHasBeenReaped(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testExitCommand(t, 0)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}

	select {
	case <-cmd.Done():
		t.Fatal("Done() was closed before the command started")
	default:
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitDone(t, cmd)

	if cmd.IsRunning() {
		t.Error("IsRunning() is true after Done() closed")
	}
}

func TestExitCodeReportsTheStatusTheChildExitedWith(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testExitCommand(t, 3)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}

	// Before the command has run there is no status to report, and the caller
	// has Done() to tell it so.
	if got := cmd.ExitCode(); got != -1 {
		t.Errorf("ExitCode() = %d before Start, want -1", got)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitDone(t, cmd)

	if got := cmd.ExitCode(); got != 3 {
		t.Errorf("ExitCode() = %d, want 3", got)
	}
}

func TestExitCodeAgreesWithTheProcessState(t *testing.T) {
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
	if err := cmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitDone(t, cmd)

	// How a terminated child's status is spelled is the platform's business;
	// ExitCode only promises to report the same thing os.ProcessState does.
	state := cmd.GetProcessState()
	if state == nil {
		t.Fatal("GetProcessState() = nil after Done() closed")
	}
	if got, want := cmd.ExitCode(), state.ExitCode(); got != want {
		t.Errorf("ExitCode() = %d, GetProcessState().ExitCode() = %d", got, want)
	}
}

func TestWithOnExitRunsBeforeDoneCloses(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	var ran bool
	var code int
	name, args := testExitCommand(t, 5)
	cmd, err := pm.NewExecCmd(name, args, procman.WithOnExit(func(c *procman.ExecCmd, _ error) {
		ran = true
		code = c.ExitCode()
	}))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitDone(t, cmd)

	// No synchronisation on ran or code: the point of the hook is that it has
	// already finished by the time Done() is closed.
	if !ran {
		t.Fatal("the exit hook had not run when Done() closed")
	}
	if code != 5 {
		t.Errorf("the hook saw ExitCode() = %d, want 5", code)
	}
}

func TestWithOnStartRunsWithARunningCommand(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	started := make(chan int, 1)
	name, args := testSleepCommand(t, 30)
	cmd, err := pm.NewExecCmd(name, args, procman.WithOnStart(func(c *procman.ExecCmd) {
		started <- c.Pid()
	}))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Stop() })

	select {
	case pid := <-started:
		if pid == 0 {
			t.Error("the start hook saw Pid() = 0, want the running child's pid")
		}
	default:
		t.Fatal("the start hook had not run when Start returned")
	}
}

// A hook is user code, and user code will call back in. It must not run holding
// a lock Procman needs, nor before the reaper it might wait on exists.
func TestWithOnStartMayCallBackIntoTheManager(t *testing.T) {
	pm := procman.NewProcman()
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testExitCommand(t, 0)
	var registerErr error
	cmd, err := pm.NewExecCmd(name, args, procman.WithOnStart(func(c *procman.ExecCmd) {
		_, registerErr = pm.NewExecCmd(name, args)
		<-c.Done()
	}))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}

	returned := make(chan error, 1)
	go func() { returned <- cmd.Start() }()

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return: the start hook is deadlocked")
	}
	if registerErr != nil {
		t.Errorf("registering a command from the start hook: %v", registerErr)
	}
}

// The manager-wide callbacks share one event loop, so one slow consumer stalls
// every other. A per-command hook that depended on that loop would inherit the
// stall, and the drops that follow it.
func TestPerCommandHooksDoNotDependOnTheEventLoop(t *testing.T) {
	const commands = 20

	release := make(chan struct{})
	pm := procman.NewProcman()
	pm.OnExit = func(*procman.ExecCmd, error) { <-release }
	t.Cleanup(func() {
		close(release)
		_ = pm.Shutdown()
	})

	var mu sync.Mutex
	seen := make(map[procman.ID]int, commands)

	cmds := make([]*procman.ExecCmd, 0, commands)
	for i := range commands {
		name, args := testExitCommand(t, i%128)
		cmd, err := pm.NewExecCmd(name, args, procman.WithOnExit(func(c *procman.ExecCmd, _ error) {
			mu.Lock()
			seen[c.ID()] = c.ExitCode()
			mu.Unlock()
		}))
		if err != nil {
			t.Fatalf("NewExecCmd: %v", err)
		}
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	for _, cmd := range cmds {
		awaitDone(t, cmd)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != commands {
		t.Fatalf("%d of %d exit hooks ran while the event loop was blocked", len(seen), commands)
	}
	for i, cmd := range cmds {
		if got := seen[cmd.ID()]; got != i%128 {
			t.Errorf("hook for command %d saw exit code %d, want %d", i, got, i%128)
		}
	}
}

// A hook that blocks is the caller's problem, but it has to stay the caller's
// problem for one command rather than everybody's.
func TestABlockingExitHookDelaysOnlyItsOwnCommand(t *testing.T) {
	pm := procman.NewProcman()
	release := make(chan struct{})
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testExitCommand(t, 0)
	blocked, err := pm.NewExecCmd(name, args, procman.WithOnExit(func(*procman.ExecCmd, error) {
		<-release
	}))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	free, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}

	if err := blocked.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := free.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	awaitDone(t, free)
	select {
	case <-blocked.Done():
		t.Fatal("the blocked command's Done() closed while its hook was still running")
	default:
	}

	close(release)
	awaitDone(t, blocked)
}

func TestPerCommandHooksAreInheritedFromTheProcmanDefaults(t *testing.T) {
	exited := make(chan procman.ID, 1)
	pm, err := procman.NewProcmanWithOptions(
		procman.WithDefaultExecCmdOptions(
			procman.WithOnExit(func(c *procman.ExecCmd, _ error) { exited <- c.ID() }),
		),
	)
	if err != nil {
		t.Fatalf("NewProcmanWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = pm.Shutdown() })

	name, args := testExitCommand(t, 0)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitDone(t, cmd)

	select {
	case id := <-exited:
		if id != cmd.ID() {
			t.Errorf("the inherited hook reported %v, want %v", id, cmd.ID())
		}
	default:
		t.Fatal("the inherited exit hook did not run")
	}
}
