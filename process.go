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

	// group holds the platform group/container handle (Job Object on
	// Windows; nil on Unix where the process group is kernel-managed).
	group groupHandle

	// wd is the Unix sidecar watchdog, if one was spawned for this
	// generation. nil on Windows or when Watchdog is off.
	wdCmd  *exec.Cmd
	wdPipe *os.File
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
// result. Spawn ordering when the watchdog is enabled is: watchdog → target
// → write pgid (arm). The watchdog is a sidecar, not in the exec chain, so
// the parent reaps the target directly and its exit code/signal are real.
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

	// Spawn ordering: watchdog FIRST (before the target), so a parent death
	// at any point after this has a watcher that knows nothing to kill yet.
	wdCmd, wdPipe, err := p.sup.spawnWatchdog(p.spec)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.wdCmd = wdCmd
	p.wdPipe = wdPipe
	p.mu.Unlock()

	p.mu.Lock()
	p.gen++
	p.pid = 0
	p.state = StateStarting
	p.startedAt = time.Now().UTC()
	p.mu.Unlock()

	if err := cmd.Start(); err != nil {
		// Target never started: stand the watchdog down (it reads EOF or the
		// stand-down line and exits 0). Close the pipe so the watchdog does
		// not linger.
		standDownWatchdog(wdPipe)
		p.mu.Lock()
		p.wdCmd = nil
		p.wdPipe = nil
		p.mu.Unlock()
		return nil, err
	}

	// Arm the watchdog: write the target's pgid down fd 3. After this, EOF
	// on the pipe (parent death) triggers the kill sequence. The pgid equals
	// the target's pid (Setpgid). There is a sub-millisecond window between
	// target Start and this write — the minimum possible since the pgid does
	// not exist until the target does.
	pgid := cmd.Process.Pid
	if wdPipe != nil {
		if armErr := armWatchdog(wdPipe, pgid); armErr != nil {
			// Arming failed: kill the target we just started and fail Start,
			// rather than proceeding with an unarmed watchdog.
			_ = signalKillGroup(cmd.Process, pgid)
			_, _ = cmd.Process.Wait()
			standDownWatchdog(wdPipe)
			return nil, armErr
		}
	}

	// Assign the child to its group/container (Job Object on Windows). On
	// Unix this returns nil since Setpgid already established the group.
	// There is a small unprotected window here on Windows between Start and
	// assignment; documented in the plan (T8).
	gh, err := assignToGroupHandle(cmd)
	if err != nil {
		// Assignment failed: kill the child we just started and fail Start,
		// rather than proceeding without the parent-death guarantee.
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		standDownWatchdog(wdPipe)
		p.mu.Lock()
		p.wdCmd = nil
		p.wdPipe = nil
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Lock()
	p.pid = cmd.Process.Pid
	p.state = StateRunning
	p.group = gh
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

	// Stand the watchdog down: the target has exited, so write the
	// stand-down line and close the sentinel pipe. The watchdog reads the
	// line and exits 0 without killing the group. Reap its exit so it does
	// not become a zombie.
	p.mu.Lock()
	wd := p.wdCmd
	wdPipe := p.wdPipe
	p.wdCmd = nil
	p.wdPipe = nil
	p.mu.Unlock()
	standDownWatchdog(wdPipe)
	if wd != nil {
		_, _ = wd.Process.Wait()
	}

	// Release the platform group/container handle (Job Object on Windows).
	// On Unix this is nil. Closing after the process has exited is safe.
	p.mu.Lock()
	gh := p.group
	p.group = nil
	p.mu.Unlock()
	if gh != nil {
		gh.close()
	}

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
//
// Ordering contract: OnExit fires BEFORE Done() closes, so a caller waiting
// on Done() can rely on OnExit having already run. OnExit runs on the reaper
// goroutine and must not block.
func (p *Process) publishExit(info ExitInfo) {
	p.mu.Lock()
	p.pid = 0
	isTerminal := true
	p.setStateLocked(StateExited)
	onExit := p.sup.opts.OnExit
	if isTerminal && !p.closed {
		p.exitInfo = info
		p.exitOk = true
		p.closed = true
	}
	p.mu.Unlock()

	if isTerminal && onExit != nil {
		onExit(p, info)
	}

	p.mu.Lock()
	if isTerminal && p.closed {
		close(p.done)
	}
	p.mu.Unlock()

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
	gh := p.group
	p.mu.Unlock()
	if err != nil {
		// FindProcess never errors on Unix; nil is safe on Windows.
		return nil
	}
	// Graceful stop of the whole group. On Unix this is kill(-pgid, SIGTERM);
	// on Windows the Job Object has no graceful mode, so we terminate the
	// whole job (hard kill) — there is no SIGTERM equivalent. ESRCH / already
	// gone is not an error: the reaper is handling it.
	if gh != nil {
		if err := gh.terminate(); err != nil {
			if !isAlreadyGone(err) {
				_ = err
			}
		}
	} else if err := signalTermGroup(proc, pid); err != nil {
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

	// Escalation: the child ignored the graceful signal (Unix), or the Job
	// Object termination did not complete in time (Windows).
	p.mu.Lock()
	pid = p.pid
	if pid == 0 {
		p.mu.Unlock()
		return nil
	}
	proc2, err := os.FindProcess(pid)
	gh2 := p.group
	p.mu.Unlock()
	if err != nil {
		return nil
	}
	var killErr error
	if gh2 != nil {
		killErr = gh2.terminate()
	} else {
		killErr = signalKillGroup(proc2, pid)
	}
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