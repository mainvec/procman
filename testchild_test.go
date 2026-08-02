package procman_test

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// testChildCmd builds an *exec.Command that re-executes the test binary as a
// test child with the given flags. It does NOT start it.
func testChildCmd(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := procman.TestChildExe()
	if err != nil {
		t.Fatalf("TestChildExe: %v", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(cmd.Env, procman.TestChildEnv()...)
	return cmd
}

func TestChildExitCode(t *testing.T) {
	cmd := testChildCmd(t, "-exit-code=3")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got success; output:\n%s", out)
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Fatalf("expected exit code 3, got %v; output:\n%s", err, out)
	}
}

func TestChildDelay(t *testing.T) {
	cmd := testChildCmd(t, "-exit-code=0", "-delay=100ms")
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if d := time.Since(start); d < 90*time.Millisecond {
		t.Fatalf("expected >=100ms delay, got %v", d)
	}
}

func TestChildIgnoreSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM escalation is Unix-only")
	}
	cmd := testChildCmd(t, "-exit-code=0", "-delay=5s", "-ignore-term")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Give the child time to install the handler.
	time.Sleep(100 * time.Millisecond)
	pid := cmd.Process.Pid

	// SIGTERM should be ignored: the child should still be alive shortly after.
	_ = cmd.Process.Signal(sigTERM())
	time.Sleep(200 * time.Millisecond)
	if !processAlive(pid) {
		t.Fatalf("child died despite ignoring SIGTERM")
	}
	// Clean up with SIGKILL.
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

func TestChildStdoutLines(t *testing.T) {
	cmd := testChildCmd(t, "-exit-code=0", "-stdout-lines=5")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 stdout lines, got %d: %q", len(lines), out.String())
	}
	for i, l := range lines {
		want := "stdout-" + itoa(i)
		if l != want {
			t.Fatalf("line %d = %q, want %q", i, l, want)
		}
	}
}

func TestChildStderrLines(t *testing.T) {
	cmd := testChildCmd(t, "-exit-code=0", "-stderr-lines=3")
	var out bytes.Buffer
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 stderr lines, got %d: %q", len(lines), out.String())
	}
	for i, l := range lines {
		want := "stderr-" + itoa(i)
		if l != want {
			t.Fatalf("line %d = %q, want %q", i, l, want)
		}
	}
}

func TestChildLongLine(t *testing.T) {
	// 1 MiB line, larger than bufio.Scanner's 64 KiB default. The child must
	// not deadlock; it writes and exits.
	cmd := testChildCmd(t, "-exit-code=0", "-long-line=1048576")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(out.String(), "longline ") {
		t.Fatalf("missing longline prefix: %q", out.String()[:min(20, len(out.String()))])
	}
	// The line is "longline " + 1MiB of 'x' + '\n'.
	const want = len("longline ") + 1048576 + 1
	if out.Len() != want {
		t.Fatalf("expected %d bytes, got %d", want, out.Len())
	}
}

func TestChildGrandchild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("grandchild/group semantics verified on Unix in T7/T10")
	}
	cmd := testChildCmd(t, "-exit-code=0", "-delay=300ms", "-grandchild=2")
	// Read output line-by-line on a goroutine so we can detect the
	// grandchild announcements and kill the grandchildren promptly rather
	// than waiting for their inherited stdout pipe to close at the 30s
	// delay.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024), 2*1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	// Collect lines until the parent exits; the parent's -delay is 300ms so
	// the announcement lines arrive quickly.
	var pids []int
	announceDone := make(chan struct{})
	go func() {
		for l := range lines {
			if strings.HasPrefix(l, "grandchild-") && strings.Contains(l, "pid=") {
				pids = append(pids, parsePID(l))
				if len(pids) == 2 {
					close(announceDone)
				}
			}
		}
	}()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	_ = w.Close()

	select {
	case <-announceDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("grandchildren did not announce within 2s (got %d)", len(pids))
	}

	if len(pids) < 2 {
		t.Fatalf("expected 2 grandchild pids, got %d", len(pids))
	}

	// Best-effort cleanup: kill the stray grandchildren. This is only a smoke
	// test; T10 verifies containment under a real supervisor.
	for _, pid := range pids {
		_ = killByPID(pid)
	}
}

func TestChildSetsid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("setsid is Unix-only")
	}
	// The child calls setsid; we just assert it does not error and exits 0.
	cmd := testChildCmd(t, "-exit-code=0", "-setsid")
	if err := cmd.Run(); err != nil {
		t.Fatalf("setsid child failed: %v", err)
	}
}