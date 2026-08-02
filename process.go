package procman

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Process is a stable handle to a supervised process. A restart changes PID,
// StartedAt and Generation but not the handle; Done() closes only on a
// permanent stop. Methods are safe for concurrent use.
type Process struct {
	sup *Supervisor
	id  ID
	spec Spec

	mu       sync.Mutex
	pid      int
	gen      int
	state    State
	startedAt time.Time
	done     chan struct{}
	closed   bool
	exitInfo ExitInfo
	exitOk   bool

	// out captures this generation's stdout/stderr. Replaced on each
	// restart (T11) so the ring is per-generation.
	outMu sync.Mutex
	out   *outputSet
}

// Name returns the Spec.Name.
func (p *Process) Name() string { return p.spec.Name }

// PID returns the current generation's OS PID, or 0 when not running.
func (p *Process) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pid
}

// Generation returns the current generation number, starting at 1.
func (p *Process) Generation() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen
}

// State returns the current lifecycle state.
func (p *Process) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// StartedAt returns the start time of the current generation.
func (p *Process) StartedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startedAt
}

// Done returns a channel closed when the process is permanently stopped
// (RestartNever, caller Stop, or budget exhaustion).
func (p *Process) Done() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// Exit returns the final exit info and ok=true once the process has
// permanently exited; ok=false while alive or restarting.
func (p *Process) Exit() (ExitInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitInfo, p.exitOk
}

// LogTail returns the retained ring-buffer lines, in arrival order. Lines
// from stdout precede lines from stderr in the combined tail.
func (p *Process) LogTail() []Line {
	p.outMu.Lock()
	defer p.outMu.Unlock()
	if p.out == nil {
		return nil
	}
	return p.out.tail()
}

// setStateLocked must be called with mu held.
func (p *Process) setStateLocked(s State) { p.state = s }

// startGeneration starts one process generation: launches the child, owns
// exactly one reaper goroutine that calls cmd.Wait(), and publishes the
// result. It returns the *exec.Cmd so the supervisor can signal the group.
//
// For T3 there is no watchdog and no group kill; cmd.Start is used directly.
func (p *Process) startGeneration(ctx context.Context) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, p.spec.Path, p.spec.Args...)
	cmd.Env = p.spec.Env
	cmd.Dir = p.spec.Dir
	prepareChildCmd(cmd)

	// Attach output collectors (not StdoutPipe): exec owns the pipe lifetime
	// and Wait joins the copier. A fresh collector set per generation so the
	// ring is per-generation.
	outSet := newOutputSet(p.spec)
	p.outMu.Lock()
	p.out = outSet
	p.outMu.Unlock()
	cmd.Stdout = outSet.stdout
	cmd.Stderr = outSet.stderr

	p.mu.Lock()
	p.gen++
	p.pid = 0
	p.state = StateStarting
	p.startedAt = time.Now().UTC()
	p.mu.Unlock()

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.pid = cmd.Process.Pid
	p.state = StateRunning
	p.mu.Unlock()

	// Single reaper goroutine: the only owner of cmd.Wait() for this
	// generation. Nothing else in the package calls Wait.
	go p.reap(cmd)
	return cmd, nil
}

// reap calls cmd.Wait exactly once and publishes the result.
func (p *Process) reap(cmd *exec.Cmd) {
	waitErr := cmd.Wait()

	// Flush any residual partial line now that the stream is closed, so a
	// final line without a trailing newline is not lost.
	p.outMu.Lock()
	if p.out != nil {
		p.out.flush()
	}
	p.outMu.Unlock()

	info := ExitInfo{
		ExitedAt:   time.Now().UTC(),
		Generation: p.generationAtomic(),
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			info.Code = ee.ExitCode()
			if ps := ee.ProcessState; ps != nil {
				if sig := signalFromProcessState(ps); sig != "" {
					info.Signal = sig
				}
			}
		} else {
			info.Err = waitErr
		}
	} else {
		if ps := cmd.ProcessState; ps != nil {
			info.Code = ps.ExitCode()
			if sig := signalFromProcessState(ps); sig != "" {
				info.Signal = sig
			}
		}
	}

	p.publishExit(info)
}

// publishExit records the per-generation exit, fires OnExit once, and decides
// whether the process is terminal. For T3 (RestartNever), every exit is
// terminal: Done() closes and the entry leaves the registry.
func (p *Process) publishExit(info ExitInfo) {
	p.mu.Lock()
	p.pid = 0
	isTerminal := true
	p.setStateLocked(StateExited)
	if isTerminal && !p.closed {
		p.exitInfo = info
		p.exitOk = true
		p.closed = true
		close(p.done)
	}
	onExit := p.sup.opts.OnExit
	p.mu.Unlock()

	if onExit != nil {
		onExit(p, info)
	}

	if isTerminal {
		p.sup.unregister(p)
	}
}

func (p *Process) generationAtomic() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen
}

// signalFromProcessState returns the signal name if the process was killed by
// a signal, else "". Unix-only behaviour; on Windows this returns "".
func signalFromProcessState(ps *os.ProcessState) string {
	return signalName(ps)
}

// stop signals the process to stop: sends a graceful signal, waits
// StopGrace, escalates to a hard kill if needed, and waits on the reaper
// (never on cmd.Wait directly). Idempotent and a no-op returning nil for an
// already-exited process. Returns ErrStopEscalated when a hard kill was
// required, wrapping any error from the kill itself.
func (p *Process) stop(ctx context.Context) error {
	p.mu.Lock()
	pid := p.pid
	state := p.state
	if state == StateExited || pid == 0 {
		p.mu.Unlock()
		return nil
	}
	p.setStateLocked(StateStopping)
	proc, err := os.FindProcess(pid)
	p.mu.Unlock()
	if err != nil {
		// FindProcess never errors on Unix; nil is safe on Windows.
		return nil
	}
	// Graceful signal. ESRCH (process gone) is not an error: the reaper is
	// handling it.
	if err := signalTerm(proc); err != nil {
		if !isAlreadyGone(err) {
			_ = err // best-effort; escalate below regardless
		}
	}

	// Wait up to StopGrace for a graceful exit, then escalate to a hard kill.
	grace := p.spec.StopGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
	}

	// Escalation: the child ignored the graceful signal.
	p.mu.Lock()
	pid = p.pid
	if pid == 0 {
		p.mu.Unlock()
		return nil
	}
	proc2, err := os.FindProcess(pid)
	p.mu.Unlock()
	if err != nil {
		return nil
	}
	killErr := signalKill(proc2)
	select {
	case <-p.done:
		// Escalation succeeded. Report that escalation was needed; wrap the
		// kill error if there was one (do not swallow it).
		if killErr != nil && !isAlreadyGone(killErr) {
			return fmt.Errorf("%w: %v", ErrStopEscalated, killErr)
		}
		return ErrStopEscalated
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ensure atomic package is used (state may use atomic in later tasks).
var _ = atomic.Int32{}