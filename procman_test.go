package procman_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

const (
	testSleepMode      = "--procman-sleep"
	testPrintArgv0Mode = "--procman-print-argv0"
	testExitMode       = "--procman-exit"
)

func init() {
	if len(os.Args) < 2 {
		return
	}

	switch os.Args[1] {
	case testSleepMode:
		seconds, err := strconv.Atoi(os.Args[2])
		if err != nil {
			os.Exit(2)
		}
		time.Sleep(time.Duration(seconds) * time.Second)
		os.Exit(0)
	case testExitMode:
		code, err := strconv.Atoi(os.Args[2])
		if err != nil {
			os.Exit(2)
		}
		os.Exit(code)
	case testPrintArgv0Mode:
		_, _ = os.Stdout.WriteString(os.Args[0])
		os.Exit(0)
	}
}

func testSleepCommand(t *testing.T, seconds int) (string, []string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return self, []string{testSleepMode, strconv.Itoa(seconds)}
}

func testExitCommand(t *testing.T, code int) (string, []string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return self, []string{testExitMode, strconv.Itoa(code)}
}

func testArgv0Command(t *testing.T, stdout *bytes.Buffer) (*exec.Cmd, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(self, testPrintArgv0Mode)
	const argv0 = "custom-argv0"
	cmd.Args[0] = argv0
	cmd.Stdout = stdout
	return cmd, argv0
}

func TestNewProcman(t *testing.T) {
	pm := procman.NewProcman()
	if pm == nil {
		t.Fatal("expected a procmac instance")
	}
	name, args := testSleepCommand(t, 1)
	ecmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if ecmd == nil {
		t.Fatal("expected a ExecCmd instance")
	}
	if ecmd.Name != name {
		t.Fatalf("expected Name to be %q, got: %s", name, ecmd.Name)
	}
	if !slices.Equal(ecmd.Args, args) {
		t.Fatalf("expected Args to be %v, got: %v", args, ecmd.Args)
	}

	err = ecmd.Start()
	if err != nil {
		t.Fatalf("expected no error on Start, got: %v", err)
	}
	exited := ecmd.IsExited() // should be false
	if exited {
		t.Fatalf("expected IsExited to be false, got: %v", exited)
	}

	running := ecmd.IsRunning() // should be true, but we can't assert it reliably
	if !running {
		t.Logf("expected IsRunning to be true, got: %v", running)
	}

	err = ecmd.Wait()
	if err != nil {
		t.Fatalf("expected no error on Wait, got: %v", err)
	}
	exited = ecmd.IsExited() // should be true
	if !exited {
		t.Fatalf("expected IsExited to be true, got: %v", exited)
	}
	err = ecmd.Start()
	if err == nil {
		t.Fatalf("expected error on Start after Wait, got nil")
	}

}

func TestMultipleExecCmds(t *testing.T) {
	pm := procman.NewProcman()
	var onStartCounter, onExitCounter atomic.Int32
	pm.OnStart = func(ecmd *procman.ExecCmd) {
		t.Logf("OnStart callback: ExecCmd %s[%v] started with PID: %d", ecmd.Name, ecmd.ID(), ecmd.Pid())
		onStartCounter.Add(1)
	}
	pm.OnExit = func(ecmd *procman.ExecCmd, err error) {
		t.Logf("OnExit callback: ExecCmd %s[%v] exited with error: %v", ecmd.Name, ecmd.ID(), err)
		onExitCounter.Add(1)
	}

	if pm == nil {
		t.Fatal("expected a procmac instance")
	}
	name1, args1 := testSleepCommand(t, 1)
	ecmd1, err := pm.NewExecCmd(name1, args1)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	name2, args2 := testSleepCommand(t, 2)
	ecmd2, err := pm.NewExecCmd(name2, args2)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	err = ecmd1.Start()
	if err != nil {
		t.Fatalf("expected no error on Start, got: %v", err)
	}
	err = ecmd2.Start()
	if err != nil {
		t.Fatalf("expected no error on Start, got: %v", err)
	}
	// Wait for both commands to finish

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := ecmd1.Wait(); err != nil {
			t.Errorf("expected no error on Wait, got: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := ecmd2.Wait(); err != nil {
			t.Errorf("expected no error on Wait, got: %v", err)
		}
	}()
	wg.Wait()
	pm.WaitEventLoop()
	if onStartCounter.Load() != 2 {
		t.Fatalf("expected OnStart to be called 2 times, got: %d", onStartCounter.Load())
	}
	if onExitCounter.Load() != 2 {
		t.Fatalf("expected OnExit to be called 2 times, got: %d", onExitCounter.Load())
	}

}

func TestStop(t *testing.T) {
	pm := procman.NewProcman()
	var onExitCounter atomic.Int32
	pm.OnExit = func(ecmd *procman.ExecCmd, err error) {
		onExitCounter.Add(1)
	}

	// Start a long-running process with a 5s grace period.
	name, args := testSleepCommand(t, 60)
	ecmd, err := pm.NewExecCmd(name, args,
		procman.WithGracePeriod(5*time.Second))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := ecmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Give the process a moment to start.
	time.Sleep(100 * time.Millisecond)
	if !ecmd.IsRunning() {
		t.Fatal("expected process to be running")
	}

	// Stop triggers SIGTERM, then auto-escalates to SIGKILL after 5s.
	if err := ecmd.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !ecmd.IsExited() {
		t.Fatal("expected Stop to wait for process exit")
	}

	// The process should have been terminated.
	if err := ecmd.Wait(); err == nil {
		t.Fatal("expected Wait to return non-nil error for killed process")
	}
	if ecmd.IsRunning() {
		t.Fatal("expected process to not be running after Stop")
	}
	if !ecmd.IsExited() {
		t.Fatal("expected process to be exited after Stop")
	}
	pm.WaitEventLoop()
	if onExitCounter.Load() != 1 {
		t.Fatalf("expected OnExit 1 time, got %d", onExitCounter.Load())
	}
}

func TestStopAll(t *testing.T) {
	pm := procman.NewProcman()
	cmds := make([]*procman.ExecCmd, 0, 2)
	for range 2 {
		name, args := testSleepCommand(t, 60)
		cmd, err := pm.NewExecCmd(name, args,
			procman.WithGracePeriod(100*time.Millisecond))
		if err != nil {
			t.Fatalf("NewExecCmd: %v", err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		cmds = append(cmds, cmd)
	}

	stopResult := make(chan error, 1)
	go func() {
		stopResult <- pm.StopAll()
	}()

	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("StopAll: %v", err)
		}
	case <-time.After(3 * time.Second):
		pm.KillAll()
		t.Fatal("StopAll did not return after processes exited")
	}

	for _, cmd := range cmds {
		if !cmd.IsExited() {
			t.Fatalf("expected command %v to be exited", cmd.ID())
		}
		if cmd.IsRunning() {
			t.Fatalf("expected command %v to not be running", cmd.ID())
		}
	}
}

func TestStopAllIgnoresUnstartedCommands(t *testing.T) {
	pm := procman.NewProcman()
	name, args := testSleepCommand(t, 60)
	if _, err := pm.NewExecCmd(name, args); err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := pm.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
}

func TestShutdownStopsProcessesAndRejectsNewCommands(t *testing.T) {
	pm := procman.NewProcman()
	var onExitCounter atomic.Int32
	pm.OnExit = func(*procman.ExecCmd, error) {
		onExitCounter.Add(1)
	}

	name, args := testSleepCommand(t, 60)
	cmd, err := pm.NewExecCmd(name, args,
		procman.WithGracePeriod(100*time.Millisecond))
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := pm.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !cmd.IsExited() {
		t.Fatal("expected Shutdown to wait for process exit")
	}
	if got := onExitCounter.Load(); got != 1 {
		t.Fatalf("expected Shutdown to drain exit callback, got %d callbacks", got)
	}
	if _, err := pm.NewExecCmd(name, args); !errors.Is(err, procman.ErrProcmanShutdown) {
		t.Fatalf("expected ErrProcmanShutdown after Shutdown, got %v", err)
	}
	if err := pm.Shutdown(); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestShutdownPreventsExistingCommandStart(t *testing.T) {
	pm := procman.NewProcman()
	name, args := testSleepCommand(t, 60)
	cmd, err := pm.NewExecCmd(name, args)
	if err != nil {
		t.Fatalf("NewExecCmd: %v", err)
	}
	if err := pm.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := cmd.Start(); !errors.Is(err, procman.ErrProcmanShutdown) {
		t.Fatalf("expected ErrProcmanShutdown from Start, got %v", err)
	}
}

func TestShutdownIsConcurrentAndIdempotent(t *testing.T) {
	pm := procman.NewProcman()
	results := make(chan error, 4)
	for range 4 {
		go func() {
			results <- pm.Shutdown()
		}()
	}
	for range 4 {
		if err := <-results; err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}
	pm.StopEventLoop()
}

func TestNewExecCmdFromCmdPreservesArgv0(t *testing.T) {
	pm := procman.NewProcman()
	var stdout bytes.Buffer
	cmd, expectedArgv0 := testArgv0Command(t, &stdout)

	ecmd, err := pm.NewExecCmdFromCmd(cmd)
	if err != nil {
		t.Fatalf("NewExecCmdFromCmd: %v", err)
	}
	if err := ecmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := ecmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := stdout.String(); got != expectedArgv0 {
		t.Fatalf("expected argv[0] to be preserved, got %q", got)
	}
}

func TestProcessLifecycleOptions(t *testing.T) {
	pm := procman.NewProcman()
	name, args := testSleepCommand(t, 1)
	opts := []procman.ExecCmdOption{procman.WithProcessTreeTermination()}
	if _, err := pm.NewExecCmd(name, args, opts...); err != nil {
		t.Fatalf("WithProcessTreeTermination: %v", err)
	}
	if _, err := pm.NewExecCmd(name, args,
		procman.WithParentDeathCleanupIfSupported()); err != nil {
		t.Fatalf("WithParentDeathCleanupIfSupported: %v", err)
	}
	_, err := pm.NewExecCmd(name, args, procman.WithParentDeathCleanup())
	if procman.SupportsParentDeathCleanup() && err != nil {
		t.Fatalf("WithParentDeathCleanup on supported platform: %v", err)
	}
	if !procman.SupportsParentDeathCleanup() && !errors.Is(err, procman.ErrParentDeathCleanupUnsupported) {
		t.Fatalf("expected ErrParentDeathCleanupUnsupported, got %v", err)
	}
}
