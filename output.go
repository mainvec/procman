package procman

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// maxLineLen caps a single retained/emit line length so a child emitting
// binary cannot exhaust memory. The raw bytes still flow to the Spec.Stdout
// sink unmodified; only the line-callback and ring buffer truncate.
const maxLineLen = 64 * 1024

// outputCollector captures child output per stream. It implements io.Writer
// and is assigned to cmd.Stdout / cmd.Stderr so exec owns the pipe lifetime
// and Wait joins the copier (avoiding StdoutPipe's goroutine-leak hazard).
type outputCollector struct {
	stream Stream
	spec   Spec

	// raw sink receives bytes as-is.
	sink io.Writer
	// onLine fires per completed line.
	onLine func(Line)
	// ring holds the last LogTailLines lines.
	ring *lineRing

	// residual buffers a partial line between Write calls (line splitting
	// across pipe reads). Guarded by mu along with the ring.
	mu       sync.Mutex
	residual []byte
}

func newOutputCollector(stream Stream, spec Spec) *outputCollector {
	oc := &outputCollector{
		stream: stream,
		spec:   spec,
		sink:   nil,
	}
	if stream == StreamStdout {
		oc.sink = spec.Stdout
	} else {
		oc.sink = spec.Stderr
	}
	oc.onLine = spec.OnLine
	if spec.LogTailLines > 0 {
		oc.ring = newLineRing(spec.LogTailLines)
	}
	return oc
}

// Write implements io.Writer. It fans bytes out to the raw sink immediately
// and splits into lines for the callback and ring buffer.
func (oc *outputCollector) Write(p []byte) (int, error) {
	n := len(p)
	// Raw sink first, unmodified, so the caller sees exact bytes.
	if oc.sink != nil {
		// A slow sink should not block the reaper forever; but exec's copier
		// runs concurrently with Wait, so a bounded sink is the caller's
		// concern. We write synchronously to preserve ordering.
		if _, err := oc.sink.Write(p); err != nil {
			return 0, err
		}
	}

	oc.mu.Lock()
	defer oc.mu.Unlock()

	// Append to residual and split into lines, normalising CRLF to LF.
	buf := append(oc.residual, p...)
	oc.residual = nil

	// Normalise \r\n -> \n first, and lone \r -> \n (best-effort). We do this
	// in place on buf.
	buf = normalizeNewlines(buf)

	for {
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			// Keep the remaining partial line for the next Write.
			oc.residual = append(oc.residual[:0], buf...)
			break
		}
		line := buf[:idx]
		buf = buf[idx+1:]
		oc.emitLine(line)
	}
	return n, nil
}

// emitLine processes one complete line (without the trailing newline).
func (oc *outputCollector) emitLine(raw []byte) {
	text := string(raw)
	// Truncate for the callback/ring; the raw sink already got full bytes.
	if len(text) > maxLineLen {
		text = text[:maxLineLen]
	}
	line := Line{Stream: oc.stream, Text: text, At: time.Now().UTC()}
	if oc.ring != nil {
		oc.ring.add(line)
	}
	if oc.onLine != nil {
		oc.onLine(line)
	}
}

// flush emits any residual partial line on EOF. Called when the stream closes
// so a final line without a trailing newline is not lost.
func (oc *outputCollector) flush() {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if len(oc.residual) == 0 {
		return
	}
	oc.emitLine(oc.residual)
	oc.residual = nil
}

// tail returns the retained ring-buffer lines in order.
func (oc *outputCollector) tail() []Line {
	if oc.ring == nil {
		return nil
	}
	return oc.ring.snapshot()
}

// normalizeNewlines returns a copy of b with \r\n replaced by \n and lone \r
// replaced by \n. It allocates only when a carriage return is present.
func normalizeNewlines(b []byte) []byte {
	if !bytes.ContainsAny(b, "\r") {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == '\r' {
			if i+1 < len(b) && b[i+1] == '\n' {
				// CRLF -> LF; skip the \r, the \n is added next iteration.
				continue
			}
			// lone \r -> \n
			out = append(out, '\n')
			continue
		}
		out = append(out, c)
	}
	return out
}

// lineRing is a bounded ring buffer of the last N lines.
type lineRing struct {
	mu   sync.Mutex
	buf  []Line
	n    int
	head int // index of the oldest entry
	full bool
}

func newLineRing(n int) *lineRing {
	return &lineRing{buf: make([]Line, n), n: n}
}

func (r *lineRing) add(l Line) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.head] = l
	r.head = (r.head + 1) % r.n
	if r.head == 0 {
		r.full = true
	}
}

func (r *lineRing) snapshot() []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		// Not yet wrapped: entries 0..head-1 are valid, in order.
		out := make([]Line, r.head)
		copy(out, r.buf[:r.head])
		return out
	}
	// Wrapped: head is the oldest; read head..n then 0..head.
	out := make([]Line, r.n)
	copy(out, r.buf[r.head:])
	copy(out[r.n-r.head:], r.buf[:r.head])
	return out
}

// outputSet holds the two stream collectors for a process generation and
// provides combined tail access.
type outputSet struct {
	stdout *outputCollector
	stderr *outputCollector
}

func newOutputSet(spec Spec) *outputSet {
	return &outputSet{
		stdout: newOutputCollector(StreamStdout, spec),
		stderr: newOutputCollector(StreamStderr, spec),
	}
}

func (s *outputSet) flush() {
	s.stdout.flush()
	s.stderr.flush()
}

func (s *outputSet) tail() []Line {
	out := s.stdout.tail()
	out = append(out, s.stderr.tail()...)
	return out
}