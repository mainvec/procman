package procman

import (
	"context"
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

// LogTail returns the retained ring-buffer lines, if any.
func (p *Process) LogTail() []Line {
	// Output capture (T5) populates this; T3 returns nil.
	return nil
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

// stop signals the process to stop. For T3 it sends SIGTERM-equivalent to the
// direct process only (group kill arrives in T4/T7/T8). Idempotent and a no-op
// for an already-exited process.
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
		return nil
	}
	if err := signalTerm(proc); err != nil {
		// Best-effort; escalate below regardless.
		_ = err
	}

	// Wait up to StopGrace, then escalate to a hard kill.
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
	if err := signalKill(proc2); err != nil {
		return err
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// ensure atomic package is used (state may use atomic in later tasks).
var _ = atomic.Int32{}