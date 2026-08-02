package procman

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TestChild is the contract for the self re-exec fake child. The test binary
// re-executes itself with PROCMAN_TEST_CHILD=1 and the flags below to model
// behaviours that the supervision tests need: a controlled exit, a delay,
// ignoring SIGTERM, writing lines, emitting an overlong line, and spawning
// long-lived grandchildren. The harness avoids /bin/sleep, which does not
// exist on Windows and cannot model these cases.
//
// This file is test-only but not build-tagged: the child modes that need
// Unix-specific behaviour (setsid, SIGTERM handling) gate themselves on
// runtime.GOOS so the file compiles everywhere and the Windows runner uses
// the subset.
//
// TryRunTestChild is the entry point. A test binary calls it at the top of
// main (or, for this package's tests, via TestMain); if the env var is set it
// runs the child behaviour and exits the process, never returning.

const testChildEnv = "PROCMAN_TEST_CHILD"

// testChildFlags are parsed from the re-executed argv. They are registered on
// a private FlagSet so they never collide with the test runner's flags.
type testChildFlags struct {
	exitCode   int
	delay      time.Duration
	ignoreTerm bool
	stdoutN    int
	stderrN    int
	longLine   int
	grandchild int
	setsid     bool
}

func parseTestChildFlags(args []string) testChildFlags {
	fs := flag.NewFlagSet("procman-testchild", flag.ExitOnError)
	var f testChildFlags
	fs.IntVar(&f.exitCode, "exit-code", 0, "exit code")
	fs.DurationVar(&f.delay, "delay", 0, "delay before exit")
	fs.BoolVar(&f.ignoreTerm, "ignore-term", false, "ignore SIGTERM")
	fs.IntVar(&f.stdoutN, "stdout-lines", 0, "number of lines to write to stdout")
	fs.IntVar(&f.stderrN, "stderr-lines", 0, "number of lines to write to stderr")
	fs.IntVar(&f.longLine, "long-line", 0, "emit one line of this many bytes to stdout")
	fs.IntVar(&f.grandchild, "grandchild", 0, "spawn N long-lived grandchildren")
	fs.BoolVar(&f.setsid, "setsid", false, "call setsid (Unix only)")
	fs.Parse(args)
	return f
}

// TryRunTestChild runs the test child behaviour when the process was
// re-executed as a test child (PROCMAN_TEST_CHILD=1). It returns true if it
// handled the invocation and the caller must not continue; the process will
// have exited. It returns false for a normal test-binary invocation.
func TryRunTestChild() bool {
	if os.Getenv(testChildEnv) == "" {
		return false
	}
	runTestChild()
	return true
}

func runTestChild() {
	f := parseTestChildFlags(os.Args[1:])

	if f.setsid {
		if err := testChildSetsid(); err != nil {
			fmt.Fprintf(os.Stderr, "testchild: setsid failed: %v\n", err)
			os.Exit(1)
		}
	}

	// Spawn grandchildren first so they are alive before any output/exit.
	var gcWG sync.WaitGroup
	for i := 0; i < f.grandchild; i++ {
		gcWG.Add(1)
		go func(i int) {
			defer gcWG.Done()
			spawnGrandchild(i)
		}(i)
	}

	// Optionally ignore SIGTERM so Stop's escalation to SIGKILL can be tested.
	// On Windows SIGTERM is not a real signal; the flag is a no-op there.
	if f.ignoreTerm {
		ignoreSIGTERM()
	}

	// Emit a line longer than bufio.Scanner's 64 KiB default, to exercise the
	// line-splitter cap. The line is one space-prefixed token so it is still a
	// single line ending in \n.
	if f.longLine > 0 {
		fmt.Fprintf(os.Stdout, "longline %s\n", strings.Repeat("x", f.longLine))
	}

	for i := 0; i < f.stdoutN; i++ {
		fmt.Fprintf(os.Stdout, "stdout-%d\n", i)
	}
	for i := 0; i < f.stderrN; i++ {
		fmt.Fprintf(os.Stderr, "stderr-%d\n", i)
	}

	// Flush before sleeping so output ordering is deterministic relative to
	// the exit. os.Stdout/os.Stderr are unbuffered on Unix; on Windows they
	// are line-buffered when connected to a pipe, and each Fprintf flushes.
	os.Stdout.Sync()
	os.Stderr.Sync()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}

	// Keep grandchildren alive until we exit. They inherit our exit; the
	// supervisor's group kill is what should terminate them.
	if f.grandchild > 0 {
		gcWG.Wait()
	}

	os.Exit(f.exitCode)
}

// spawnGrandchild starts a long-lived grandchild by re-executing the test
// binary again with a long delay and no grandchildren of its own, so it does
// not recurse. The grandchild prints one identifying line.
func spawnGrandchild(i int) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testchild: grandchild exe lookup failed: %v\n", err)
		return
	}
	cmd := exec.Command(exe, "-exit-code=0", "-delay=30s")
	cmd.Env = append(os.Environ(), testChildEnv+"=1")
	// Give the grandchild its own stdout so the parent test can see it; it is
	// intentionally NOT a pipe the supervisor owns.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	prepareGrandchildCmd(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "testchild: grandchild %d start failed: %v\n", i, err)
		return
	}
	fmt.Fprintf(os.Stdout, "grandchild-%d-pid=%d\n", i, cmd.Process.Pid)
	// Leave it running; we exit before it does and rely on the group kill.
}

// testChildExe returns the path of the current test binary, cached. Tests
// use it to build argv for the harness.
var testChildExeOnce struct {
	sync.Once
	path string
	err  error
}

// TestChildExe returns the absolute path of the running test binary, suitable
// for re-executing it as a child. It is safe to call concurrently and caches
// the result. Tests use it to drive the child harness.
func TestChildExe() (string, error) {
	testChildExeOnce.Do(func() {
		testChildExeOnce.path, testChildExeOnce.err = os.Executable()
	})
	return testChildExeOnce.path, testChildExeOnce.err
}

// TestChildArgs builds an argv slice for re-executing the test binary as a
// child with the given flags. It is a convenience for tests.
func TestChildArgs(exitCode int, delay time.Duration, extra ...string) []string {
	args := []string{
		"-exit-code=" + strconv.Itoa(exitCode),
		"-delay=" + delay.String(),
	}
	return append(args, extra...)
}

// TestChildEnv returns the environment entries needed to activate the child
// harness, to be appended to the child's environment.
func TestChildEnv() []string {
	return []string{testChildEnv + "=1"}
}