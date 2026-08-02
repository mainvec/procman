package procman_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	procman "github.com/mainvec/procman"
)

// TestOutputWriterSink verifies bytes written to stdout/stderr reach the
// Spec.Stdout/Stderr writers.
func TestOutputWriterSink(t *testing.T) {
	s := startTestSupervisor(t)
	var outMu sync.Mutex
	var out, errBuf strings.Builder
	stdoutW := &lockingWriter{mu: &outMu, w: &out}
	stderrW := &lockingWriter{mu: &outMu, w: &errBuf}
	_, err := s.Start(context.Background(), procman.Spec{
		Name:      "out",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 100*time.Millisecond, "-stdout-lines=5", "-stderr-lines=3"),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
		Stdout:    stdoutW,
		Stderr:    stderrW,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	outMu.Lock()
	defer outMu.Unlock()
	if got := strings.Count(out.String(), "stdout-"); got != 5 {
		t.Fatalf("expected 5 stdout lines in writer, got %d: %q", got, out.String())
	}
	if got := strings.Count(errBuf.String(), "stderr-"); got != 3 {
		t.Fatalf("expected 3 stderr lines in writer, got %d: %q", got, errBuf.String())
	}
}

// TestOutputOnLine verifies the OnLine callback fires per line with correct
// stream tags.
func TestOutputOnLine(t *testing.T) {
	s := startTestSupervisor(t)
	var mu sync.Mutex
	var lines []procman.Line
	onLine := func(l procman.Line) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, l)
	}
	_, err := s.Start(context.Background(), procman.Spec{
		Name:      "lines",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 100*time.Millisecond, "-stdout-lines=3", "-stderr-lines=2"),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
		OnLine:    onLine,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	var stdoutN, stderrN int
	for _, l := range lines {
		if l.Stream == procman.StreamStdout {
			stdoutN++
			if !strings.HasPrefix(l.Text, "stdout-") {
				t.Errorf("stdout line text = %q", l.Text)
			}
		} else if l.Stream == procman.StreamStderr {
			stderrN++
			if !strings.HasPrefix(l.Text, "stderr-") {
				t.Errorf("stderr line text = %q", l.Text)
			}
		}
	}
	if stdoutN != 3 {
		t.Fatalf("expected 3 stdout lines via OnLine, got %d", stdoutN)
	}
	if stderrN != 2 {
		t.Fatalf("expected 2 stderr lines via OnLine, got %d", stderrN)
	}
}

// TestOutputRingTail verifies LogTailLines retains exactly the last N lines in
// order with correct stream tags, across many lines.
func TestOutputRingTail(t *testing.T) {
	s := startTestSupervisor(t)
	p, err := s.Start(context.Background(), procman.Spec{
		Name:         "tail",
		Path:         testChildExe(t),
		Args:         procman.TestChildArgs(0, 200*time.Millisecond, "-stdout-lines=10000"),
		Env:          procman.TestChildEnv(),
		StopGrace:    time.Second,
		LogTailLines: 100,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close")
	}
	tail := p.LogTail()
	if len(tail) != 100 {
		t.Fatalf("expected 100 tail lines, got %d", len(tail))
	}
	// The last 100 of 0..9999 are 9900..9999.
	for i, l := range tail {
		want := "stdout-" + itoa(9900+i)
		if l.Text != want {
			t.Fatalf("tail[%d] = %q, want %q", i, l.Text, want)
		}
		if l.Stream != procman.StreamStdout {
			t.Fatalf("tail[%d] stream = %v, want stdout", i, l.Stream)
		}
	}
}

// TestOutputLongLine verifies a 1 MiB line does not deadlock the child and is
// truncated (not dropped).
func TestOutputLongLine(t *testing.T) {
	s := startTestSupervisor(t)
	var outMu sync.Mutex
	var out strings.Builder
	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "long",
		Path:      testChildExe(t),
		Args:      procman.TestChildArgs(0, 100*time.Millisecond, "-long-line=1048576"),
		Env:       procman.TestChildEnv(),
		StopGrace: time.Second,
		Stdout:    &lockingWriter{mu: &outMu, w: &out},
		LogTailLines: 10,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close (child may have deadlocked on full pipe)")
	}
	tail := p.LogTail()
	if len(tail) == 0 {
		t.Fatal("expected at least one tail line for the long line")
	}
	// The long line must be present, truncated to a bounded length, not dropped.
	first := tail[0]
	if !strings.HasPrefix(first.Text, "longline ") {
		t.Fatalf("first tail line = %q, want longline prefix", first.Text)
	}
	// Truncation: the retained line is bounded, well under the 1 MiB emitted.
	if len(first.Text) >= 1048576 {
		t.Fatalf("long line was not truncated: len=%d", len(first.Text))
	}
}

// TestOutputCRLF verifies \r\n is normalized to \n in line output.
func TestOutputCRLF(t *testing.T) {
	// The test child writes plain \n lines; to test CRLF we exec a small
	// printf via sh on Unix.
	if runtimeGOOS == "windows" {
		t.Skip("sh-based CRLF test is Unix-only")
	}
	s := startTestSupervisor(t)
	var mu sync.Mutex
	var lines []procman.Line
	onLine := func(l procman.Line) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, l)
	}
	// Use /bin/sh to emit a CRLF-terminated line.
	_, err := s.Start(context.Background(), procman.Spec{
		Name:      "crlf",
		Path:      "/bin/sh",
		Args:      []string{"-c", "printf 'hello\\r\\nworld\\r\\n'"},
		StopGrace: time.Second,
		OnLine:    onLine,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %+v", len(lines), lines)
	}
	if lines[0].Text != "hello" {
		t.Fatalf("line 0 = %q, want %q (CRLF not normalized)", lines[0].Text, "hello")
	}
	if lines[1].Text != "world" {
		t.Fatalf("line 1 = %q, want %q", lines[1].Text, "world")
	}
}

// lockingWriter is a thread-safe io.Writer wrapping another writer.
type lockingWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (lw *lockingWriter) Write(b []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(b)
}