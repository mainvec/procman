package procman

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

var (
	// ErrExecCmdNotStarted indicates that an operation requiring a successfully
	// started child was called before Start completed.
	ErrExecCmdNotStarted = errors.New("child process not started")
	// ErrExecCmdAlreadyStarted indicates that Start was called more than once.
	ErrExecCmdAlreadyStarted = errors.New("child process already started")
	// ErrProcmanShutdown indicates that command creation or start was attempted
	// after the Procman began shutting down.
	ErrProcmanShutdown = errors.New("procman is shutdown")
	// ErrParentDeathCleanupUnsupported indicates that the current platform
	// cannot provide native cleanup after the parent process exits.
	ErrParentDeathCleanupUnsupported = errors.New("parent-death cleanup is unsupported on this platform")
	// ErrExecCmdNotFound indicates that no registered command has the given ID.
	ErrExecCmdNotFound = errors.New("child process not found")
	// ErrExecCmdActive indicates that a command cannot be removed because its
	// direct child is running or its process tree is still tracked.
	ErrExecCmdActive = errors.New("child process still active")
	// ErrExecCmdRemoved indicates that Start was called on a command that had
	// been removed from its Procman before it started.
	ErrExecCmdRemoved = errors.New("child process removed")
	// ErrStopSignalUnsupported indicates that WithStopSignal was used on a
	// platform where the graceful stop signal cannot be chosen.
	ErrStopSignalUnsupported = errors.New("stop signal cannot be configured on this platform")
)

// ExecCmdOption configures a command managed by Procman.
type ExecCmdOption func(*execCmdOptions) error

type execCmdOptions struct {
	gracePeriod            time.Duration
	processTreeTermination bool
	parentDeathCleanup     bool
	orphanCleanup          bool
	autoRemove             bool
	stopSignal             syscall.Signal
	onStart                func(*ExecCmd)
	onExit                 func(*ExecCmd, error)
	// future fields: Env, Dir, Stdout, Stderr, etc.
}

// ProcmanOption configures a Procman during construction.
type ProcmanOption func(*procmanOptions) error

type procmanOptions struct {
	defaultExecCmdOptions execCmdOptions
}

// WithDefaultExecCmdOptions sets options inherited by every command created by
// the Procman. Command-specific options are applied afterward. Scalar settings
// can override defaults; inherited enable-only settings remain enabled.
func WithDefaultExecCmdOptions(opts ...ExecCmdOption) ProcmanOption {
	return func(o *procmanOptions) error {
		for _, opt := range opts {
			if err := opt(&o.defaultExecCmdOptions); err != nil {
				return err
			}
		}
		return nil
	}
}

// WithGracePeriod sets how long Stop waits after its graceful signal before
// forcing termination. A zero duration, the default, disables forced
// escalation and waits indefinitely for graceful exit.
func WithGracePeriod(d time.Duration) ExecCmdOption {
	return func(o *execCmdOptions) error {
		if d < 0 {
			return errors.New("grace period cannot be negative")
		}
		o.gracePeriod = d
		return nil
	}
}

// WithProcessTreeTermination makes explicit stop operations target the
// command's process tree instead of only its direct process. Unix uses a
// process group and Windows uses a Job Object.
func WithProcessTreeTermination() ExecCmdOption {
	return func(o *execCmdOptions) error {
		o.processTreeTermination = true
		return nil
	}
}

// WithOrphanCleanup terminates the descendants a command's direct child leaves
// behind when it exits on its own, rather than leaving them for a later Stop,
// StopAll, KillAll or Shutdown. They are stopped as Stop would: a graceful
// signal, the command's grace period, then forced termination. It requires
// WithProcessTreeTermination, which defines the tree; command creation fails
// without it. It does not act when the exit was requested through Stop or
// KillAll, which handle the tree themselves.
func WithOrphanCleanup() ExecCmdOption {
	return func(o *execCmdOptions) error {
		o.orphanCleanup = true
		return nil
	}
}

// WithAutoRemove removes the command from its Procman's registry once it has
// finished: after its direct child has been reaped and nothing is left in its
// tree, just before TreeDone closes. Without it, finished commands stay
// registered, with everything they reference, until Remove is called. Holders
// of the *ExecCmd can still use it after removal. Procman.OnExit callbacks are
// delivered asynchronously and may run after the command has been removed.
func WithAutoRemove() ExecCmdOption {
	return func(o *execCmdOptions) error {
		o.autoRemove = true
		return nil
	}
}

// WithStopSignal sets the graceful signal sent by Stop, StopAll, Shutdown and
// orphan cleanup, in place of SIGTERM. With WithProcessTreeTermination it goes
// to the whole process group, including what remains of a tracked tree. Forced
// termination is still SIGKILL. SIGKILL and SIGSTOP are rejected: neither can
// be handled, so neither is a graceful request. On Windows, which has no
// group-addressable alternative to CTRL_BREAK_EVENT, it returns
// ErrStopSignalUnsupported.
func WithStopSignal(sig syscall.Signal) ExecCmdOption {
	return func(o *execCmdOptions) error {
		if err := validateStopSignal(sig); err != nil {
			return err
		}
		o.stopSignal = sig
		return nil
	}
}

// WithParentDeathCleanup requests native cleanup when the application running
// Procman exits without calling Stop or Shutdown. It returns
// ErrParentDeathCleanupUnsupported on unsupported platforms.
func WithParentDeathCleanup() ExecCmdOption {
	return func(o *execCmdOptions) error {
		if !SupportsParentDeathCleanup() {
			return ErrParentDeathCleanupUnsupported
		}
		o.parentDeathCleanup = true
		return nil
	}
}

// WithParentDeathCleanupIfSupported enables native parent-death cleanup when
// available and otherwise leaves it disabled without returning an error. Use
// SupportsParentDeathCleanup when the application needs to report whether the
// cleanup guarantee is active.
func WithParentDeathCleanupIfSupported() ExecCmdOption {
	return func(o *execCmdOptions) error {
		if SupportsParentDeathCleanup() {
			o.parentDeathCleanup = true
		}
		return nil
	}
}

// WithOnStart registers a callback run once the command is running, from the
// goroutine that called Start, after the reaper has been started and with no
// manager lock held. Unlike Procman.OnStart it is never dropped, and it delays
// Start's return until it completes.
func WithOnStart(fn func(*ExecCmd)) ExecCmdOption {
	return func(o *execCmdOptions) error {
		o.onStart = fn
		return nil
	}
}

// WithOnExit registers a callback run once the command has exited and been
// reaped, from that command's own reaper goroutine, with the error returned by
// exec.Cmd.Wait. Unlike Procman.OnExit it is never dropped: it is called
// directly rather than queued, and the command's Done channel is not closed
// until it returns. A callback that blocks therefore delays Done, Wait and
// Shutdown for that one command, and one that panics ends the process: it runs
// on the reaper goroutine, where no caller can recover for it. The callback
// must not call Wait or Shutdown, because both wait for the callback to return.
func WithOnExit(fn func(*ExecCmd, error)) ExecCmdOption {
	return func(o *execCmdOptions) error {
		o.onExit = fn
		return nil
	}
}

// ExecCmdStatus describes the lifecycle state of an ExecCmd.
type ExecCmdStatus int

// ProcmanStatus describes whether a Procman accepts new commands.
type ProcmanStatus string

const (
	// ProcmanStatusRunning indicates that commands may be created and started.
	ProcmanStatusRunning ProcmanStatus = "running"
	// ProcmanStatusShutdown indicates that shutdown has begun.
	ProcmanStatusShutdown ProcmanStatus = "shutdown"
)

const (
	// ExecCmdStatusNotStarted indicates that Start has not succeeded.
	ExecCmdStatusNotStarted ExecCmdStatus = iota
	// ExecCmdStatusRunning indicates that the child process is running.
	ExecCmdStatusRunning
	// ExecCmdStatusExited indicates that the child exited successfully.
	ExecCmdStatusExited
	// ExecCmdStatusFailed indicates that start failed or the child exited with an error.
	ExecCmdStatusFailed
)

// Procman owns a registry of commands and coordinates their lifecycle events.
// A Procman must be constructed with NewProcman or NewProcmanWithOptions.
type Procman struct {
	mu                    sync.RWMutex
	execCmds              map[ID]*ExecCmd
	defaultExecCmdOptions execCmdOptions
	// OnStart is called asynchronously after a child starts successfully.
	// Set it before starting commands and keep the callback non-blocking. It is
	// dropped when the event loop falls behind; use WithOnStart where every
	// start must be seen.
	OnStart func(*ExecCmd)
	// OnExit is called asynchronously after a child exits and is reaped. Its
	// error is the result of exec.Cmd.Wait. Set it before starting commands. It
	// is dropped when the event loop falls behind; use WithOnExit where every
	// exit must be seen.
	OnExit func(*ExecCmd, error)

	evtCh        chan event
	evtDone      chan struct{}
	evtInFlight  sync.WaitGroup // tracks pending events for WaitEventLoop
	evtMu        sync.RWMutex
	evtStopped   bool
	logger       *slog.Logger
	status       ProcmanStatus
	shutdownOnce sync.Once
	shutdownErr  error
	// shutdownDone closes when the shutdown run has finished. shutdownCancel
	// escalates that run to forced termination. Both are set inside
	// shutdownOnce.
	shutdownDone   chan struct{}
	shutdownCancel context.CancelFunc
}

type event struct {
	kind int // 0=start, 1=exit
	ecmd *ExecCmd
	err  error
}

// ExecCmd is a single-start handle to a child process managed by Procman.
// Its methods are safe for concurrent use unless otherwise documented.
type ExecCmd struct {
	mu sync.RWMutex
	// Name is the command path or caller-supplied argv[0].
	Name string
	// Args contains command arguments excluding argv[0].
	Args                   []string
	procman                *Procman
	procmanId              ID
	pid                    int
	cmd                    *exec.Cmd
	started                bool
	stopping               bool
	stopRequested          bool
	startedAt              time.Time
	exitedAt               time.Time
	status                 ExecCmdStatus
	waitErr                error
	doneChan               chan struct{}
	stopDone               chan struct{}
	stopErr                error
	gracePeriod            time.Duration
	processTreeTermination bool
	parentDeathCleanup     bool
	orphanCleanup          bool
	autoRemove             bool
	stopSignal             syscall.Signal
	// removed is set once the command has left the registry. A removed command
	// that never started cannot start: nothing would be able to stop it.
	removed bool
	// treeTracked is true from the moment the reaper finds the direct child's
	// tree still populated until the tree is first observed empty. Only a
	// tracked tree may be signalled once the direct child has been reaped.
	treeTracked bool
	treeDone    chan struct{}
	// onStart and onExit are read without holding mu: both are fixed at
	// construction and never reassigned.
	onStart  func(*ExecCmd)
	onExit   func(*ExecCmd, error)
	platform platformState
}

// NewProcman creates a running Procman with default command behavior.
func NewProcman() *Procman {
	return newProcman(execCmdOptions{})
}

// NewProcmanWithOptions creates a running Procman with validated construction
// options. It returns an error when a Procman option or default command option
// is invalid or unsupported.
func NewProcmanWithOptions(opts ...ProcmanOption) (*Procman, error) {
	var o procmanOptions
	for _, opt := range opts {
		if err := opt(&o); err != nil {
			return nil, err
		}
	}
	return newProcman(o.defaultExecCmdOptions), nil
}

func newProcman(defaultExecCmdOptions execCmdOptions) *Procman {
	p := &Procman{
		execCmds:              make(map[ID]*ExecCmd),
		defaultExecCmdOptions: defaultExecCmdOptions,
		evtCh:                 make(chan event, 256), // buffered; drops supervision cost
		evtDone:               make(chan struct{}),
		status:                ProcmanStatusRunning,
		logger:                slog.Default(),
	}
	go p.eventLoop()
	return p
}

func (pm *Procman) eventLoop() {
	defer close(pm.evtDone)
	for e := range pm.evtCh {
		switch e.kind {
		case 0:
			if pm.OnStart != nil {
				pm.OnStart(e.ecmd)
			}
		case 1:
			if pm.OnExit != nil {
				pm.OnExit(e.ecmd, e.err)
			}
		}
		pm.evtInFlight.Done()
	}
}

// StopEventLoop stops callback delivery and waits for the event-loop goroutine
// to exit. It is idempotent. Events produced afterward are discarded. Most
// callers should use Shutdown, which first stops children and drains events.
func (pm *Procman) StopEventLoop() {
	pm.evtMu.Lock()
	if !pm.evtStopped {
		pm.evtStopped = true
		close(pm.evtCh)
	}
	pm.evtMu.Unlock()
	<-pm.evtDone
}

// non-blocking send; never holds c.mu when called
func (pm *Procman) notifyOnStart(ecmd *ExecCmd) {
	pm.evtMu.RLock()
	defer pm.evtMu.RUnlock()
	if pm.evtStopped {
		return
	}
	pm.evtInFlight.Add(1)
	select {
	case pm.evtCh <- event{kind: 0, ecmd: ecmd}:
	default:
		pm.evtInFlight.Done() // dropped — not in flight
		if pm.logger != nil {
			pm.logger.Warn("procman: dropping start event; callback queue full",
				"name", ecmd.Name, "id", ecmd.ID().String())
		}
	}
}

func (pm *Procman) notifyOnExit(ecmd *ExecCmd, err error) {
	pm.evtMu.RLock()
	defer pm.evtMu.RUnlock()
	if pm.evtStopped {
		return
	}
	pm.evtInFlight.Add(1)
	select {
	case pm.evtCh <- event{kind: 1, ecmd: ecmd, err: err}:
	default:
		pm.evtInFlight.Done()
		if pm.logger != nil {
			pm.logger.Warn("procman: dropping exit event; callback queue full",
				"name", ecmd.Name, "id", ecmd.ID().String())
		}
	}
}

// WaitEventLoop blocks until all events already accepted by the event loop have
// been processed. It does not stop the loop. Callers requiring a complete drain
// must first prevent commands from starting or exiting; Shutdown does this.
func (pm *Procman) WaitEventLoop() {
	pm.evtInFlight.Wait()
}

// NewExecCmd constructs and registers a command from an executable name and
// arguments. Procman defaults are applied first, followed by opts. The returned
// command is registered but not started.
func (p *Procman) NewExecCmd(name string, args []string, opts ...ExecCmdOption) (*ExecCmd, error) {
	cmd := exec.Command(name, args...)
	return p.NewExecCmdFromCmd(cmd, opts...)
}

// NewExecCmdFromCmd registers an unstarted caller-supplied command. Procman
// starts the exact *exec.Cmd without rebuilding it, preserving fields such as
// Env, Dir, standard streams, and argv[0]. It rejects nil or started commands.
//
// Set Stdout and Stderr to writers rather than using StdoutPipe or StderrPipe:
// the reaper calls Wait as soon as the child exits, which closes those pipes
// under whatever is reading them.
func (p *Procman) NewExecCmdFromCmd(cmd *exec.Cmd, opts ...ExecCmdOption) (*ExecCmd, error) {
	if cmd == nil {
		return nil, errors.New("cmd cannot be nil")
	}
	if cmd.Process != nil {
		return nil, errors.New("cmd already started")
	}

	o := p.defaultExecCmdOptions
	for _, opt := range opts {
		if err := opt(&o); err != nil {
			return nil, err
		}
	}
	if o.orphanCleanup && !o.processTreeTermination {
		return nil, errors.New("WithOrphanCleanup requires WithProcessTreeTermination")
	}
	// exec.Cmd runs {Path} when Args is empty, so a literal &exec.Cmd{Path: p}
	// is valid and must not be indexed.
	name, args := cmd.Path, []string(nil)
	if len(cmd.Args) > 0 {
		name, args = cmd.Args[0], cmd.Args[1:]
	}
	childId := NewID()
	child := &ExecCmd{
		Name:                   name,
		Args:                   args,
		procman:                p,
		procmanId:              childId,
		cmd:                    cmd, // store now; Start() will use it
		status:                 ExecCmdStatusNotStarted,
		doneChan:               make(chan struct{}),
		treeDone:               make(chan struct{}),
		processTreeTermination: o.processTreeTermination,
		parentDeathCleanup:     o.parentDeathCleanup,
		orphanCleanup:          o.orphanCleanup,
		autoRemove:             o.autoRemove,
		stopSignal:             o.stopSignal,
		gracePeriod:            o.gracePeriod,
		onStart:                o.onStart,
		onExit:                 o.onExit,
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status == ProcmanStatusShutdown {
		return nil, ErrProcmanShutdown
	}
	p.execCmds[childId] = child
	return child, nil
}

// GetExecCmd returns the registered command with id.
func (p *Procman) GetExecCmd(id ID) (*ExecCmd, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	child, ok := p.execCmds[id]
	return child, ok
}

// Remove removes a finished or never-started command from the registry, so
// that the Procman no longer retains it. A command is finished once TreeDone
// has closed. Remove returns ErrExecCmdNotFound for an unknown ID, and
// ErrExecCmdActive while the direct child runs or its tree is still tracked,
// because StopAll, KillAll and Shutdown find commands through the registry. A
// removed command that never started returns ErrExecCmdRemoved from Start.
// Holders of the *ExecCmd can still read its state and channels.
func (p *Procman) Remove(id ID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.execCmds[id]
	if !ok {
		return ErrExecCmdNotFound
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		select {
		case <-c.treeDone:
		default:
			return ErrExecCmdActive
		}
	}
	c.removed = true
	delete(p.execCmds, id)
	return nil
}

// autoRemove is called by the reaper once the command has finished, before
// TreeDone closes, so that anything observing TreeDone sees it removed.
func (p *Procman) autoRemove(c *ExecCmd) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.execCmds[c.procmanId] == c {
		delete(p.execCmds, c.procmanId)
	}
	c.removed = true
}

// ListExecCmdes returns a snapshot of all registered commands, including
// commands that have not started and finished commands that have not been
// removed.
func (p *Procman) ListExecCmdes() []*ExecCmd {
	p.mu.RLock()
	defer p.mu.RUnlock()
	children := make([]*ExecCmd, 0, len(p.execCmds))
	for _, child := range p.execCmds {
		children = append(children, child)
	}
	return children
}

// KillAll requests immediate, non-graceful termination of every running
// command, and of what remains of the tree of every command whose direct child
// has exited while its descendants are still tracked (see ExecCmd.TreeDone).
// It ignores termination errors and does not wait for process exit. A command
// whose tree has been observed empty is never signalled: its identifiers may
// since have been reused by an unrelated process.
func (p *Procman) KillAll() {
	p.mu.RLock()
	cmds := make([]*ExecCmd, 0, len(p.execCmds))
	for _, child := range p.execCmds {
		cmds = append(cmds, child)
	}
	p.mu.RUnlock()
	for _, child := range cmds {
		_ = child.kill()
	}
}

// kill forcibly terminates the command if its direct child is still running,
// or what remains of its tree while that is tracked. It does not wait.
func (c *ExecCmd) kill() error {
	c.mu.Lock()
	running := c.started && c.cmd != nil && c.cmd.Process != nil &&
		c.status == ExecCmdStatusRunning
	if running {
		c.stopRequested = true
	}
	tracked := c.treeTracked
	c.mu.Unlock()
	switch {
	case running:
		return forceKill(c)
	case tracked:
		return c.signalTrackedTree(false)
	}
	return nil
}

// StopAll gracefully stops all currently running commands concurrently and
// waits for them to exit, as Stop does. It includes commands whose direct
// child has exited while descendants are still tracked, and skips a command
// without process-tree termination whose direct child is already gone, even if
// Wait is still draining its output pipes. It returns the joined
// errors from commands that fail to stop. Registered commands that have not
// started are ignored. It is StopAllContext with context.Background.
func (p *Procman) StopAll() error {
	return p.StopAllContext(context.Background())
}

// StopAllContext is StopAll bounded by ctx. If ctx ends first, every stop
// still in progress is escalated as StopContext describes, and the result
// wraps ctx.Err() once, joined with any other stop errors.
func (p *Procman) StopAllContext(ctx context.Context) error {
	errs, abandoned := p.stopAll(ctx)
	if abandoned {
		errs = append([]error{abandonedError("stop", ctx)}, errs...)
	}
	return errors.Join(errs...)
}

// stopAll stops every command that needs it and reports their errors, apart
// from ctx ending, which it reports separately.
func (p *Procman) stopAll(ctx context.Context) (errs []error, abandoned bool) {
	cmds := p.ListExecCmdes()
	var (
		mu        sync.Mutex
		waitGroup sync.WaitGroup
	)
	for _, cmd := range cmds {
		if !cmd.needsStop() {
			continue
		}
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			cmdAbandoned, err := cmd.stop(ctx)
			mu.Lock()
			defer mu.Unlock()
			abandoned = abandoned || cmdAbandoned
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	waitGroup.Wait()
	return errs, abandoned
}

// Shutdown prevents new commands from being created or started, stops running
// commands, waits for started commands to be reaped, drains callbacks, and
// stops the event loop. Concurrent and repeated calls return the same result.
// It is ShutdownContext with context.Background.
func (p *Procman) Shutdown() error {
	return p.ShutdownContext(context.Background())
}

// ShutdownContext is Shutdown bounded by ctx. The manager stops accepting
// commands at once, and a single shutdown run proceeds as Shutdown describes,
// whichever call started it. Each call waits for that run or for its own ctx.
// If ctx ends first, the run is escalated: every command still running and
// every tracked tree is forcibly terminated before ShutdownContext returns an
// error wrapping ctx.Err(). It does not wait for those processes to be reaped.
// The run itself carries on in the background, and a later call, such as
// Shutdown, can wait for it to finish and return its result.
func (p *Procman) ShutdownContext(ctx context.Context) error {
	p.shutdownOnce.Do(func() {
		p.mu.Lock()
		p.status = ProcmanStatusShutdown
		p.mu.Unlock()
		runCtx, cancel := context.WithCancel(context.Background())
		p.shutdownCancel = cancel
		p.shutdownDone = make(chan struct{})
		go p.runShutdown(runCtx)
	})
	select {
	case <-p.shutdownDone:
		return p.shutdownErr
	case <-ctx.Done():
	}
	select {
	case <-p.shutdownDone:
		return p.shutdownErr
	default:
	}
	p.shutdownCancel()
	// Synchronously, so that every kill has been sent when this returns; the
	// cancelled run escalates its own stops too, but on other goroutines.
	p.KillAll()
	return abandonedError("shutdown", ctx)
}

func (p *Procman) runShutdown(ctx context.Context) {
	defer close(p.shutdownDone)
	defer p.shutdownCancel()
	// An escalation is reported to the caller whose context ended, not as the
	// run's result.
	errs, _ := p.stopAll(ctx)
	p.shutdownErr = errors.Join(errs...)
	for _, cmd := range p.ListExecCmdes() {
		if cmd.IsStarted() {
			<-cmd.doneChan
		}
	}
	p.WaitEventLoop()
	p.StopEventLoop()
}

func abandonedError(op string, ctx context.Context) error {
	return fmt.Errorf("%s did not complete: %w", op, ctx.Err())
}

// ID returns the identifier assigned when the command was registered.
func (c *ExecCmd) ID() ID {
	return c.procmanId
}

// Start starts the child process and its reaper goroutine. An ExecCmd can be
// started only once. Start returns ErrProcmanShutdown after manager shutdown
// and ErrExecCmdAlreadyStarted after a prior successful start.
func (c *ExecCmd) Start() error {
	if err := c.start(); err != nil {
		return err
	}
	// Outside start so that the hook runs with no manager lock held and with the
	// reaper already going: it may call back into Procman or into Wait.
	if c.onStart != nil {
		c.onStart(c)
	}
	return nil
}

func (c *ExecCmd) start() error {
	c.procman.mu.RLock()
	defer c.procman.mu.RUnlock()
	if c.procman.status == ProcmanStatusShutdown {
		return ErrProcmanShutdown
	}

	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return ErrExecCmdAlreadyStarted
	}
	if c.removed {
		c.mu.Unlock()
		return ErrExecCmdRemoved
	}
	cmd := c.cmd
	if err := prepareExecCmd(c); err != nil {
		c.status = ExecCmdStatusFailed
		c.mu.Unlock()
		return err
	}
	err := startProcess(c)
	if err != nil {
		cleanupExecCmd(c)
		c.status = ExecCmdStatusFailed
		c.mu.Unlock()
		return err
	}
	if err := finishExecCmdStart(c); err != nil {
		killErr := cmd.Process.Kill()
		if killErr == nil {
			_ = cmd.Wait()
		}
		cleanupExecCmd(c)
		c.status = ExecCmdStatusFailed
		c.mu.Unlock()
		return errors.Join(err, killErr)
	}

	c.started = true
	c.status = ExecCmdStatusRunning
	c.startedAt = time.Now()
	c.pid = cmd.Process.Pid
	c.mu.Unlock()
	c.procman.notifyOnStart(c)

	go func() {
		err := cmd.Wait()
		// Checked before Done closes, so that a tree with members left in it is
		// tracked from the moment anything can observe the exit.
		tracked := treeAlive(c)
		if !tracked {
			cleanupExecCmd(c)
		}
		c.mu.Lock()
		if err != nil {
			c.status = ExecCmdStatusFailed
			c.waitErr = err
		} else {
			c.status = ExecCmdStatusExited
		}
		c.exitedAt = time.Now()
		c.treeTracked = tracked
		exitedOnItsOwn := !c.stopRequested
		c.mu.Unlock()
		c.procman.notifyOnExit(c, err)
		// Before the close, so that anything waiting on Done sees a command the
		// hook has already finished with.
		if c.onExit != nil {
			c.onExit(c, err)
		}
		close(c.doneChan)
		if tracked {
			if c.orphanCleanup && exitedOnItsOwn {
				go func() { _ = c.Stop() }()
			}
			c.trackTree()
			cleanupExecCmd(c)
		}
		if c.autoRemove {
			c.procman.autoRemove(c)
		}
		close(c.treeDone)
	}()
	return nil
}

// IsRunning reports whether the command successfully started, remains in the
// running state, and appears alive to the operating system.
func (c *ExecCmd) IsRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return false
	}
	if c.status != ExecCmdStatusRunning {
		return false
	}
	if c.cmd == nil || c.cmd.Process == nil {
		return false
	}
	return processIsAlive(c.cmd.Process)
}

// Pid returns the child process ID, or zero before a successful Start.
func (c *ExecCmd) Pid() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// IsStarted reports whether Start completed successfully.
func (c *ExecCmd) IsStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// IsExited reports whether the child has exited and been reaped, successfully
// or unsuccessfully.
func (c *ExecCmd) IsExited() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return false
	}
	return c.status == ExecCmdStatusExited || c.status == ExecCmdStatusFailed
}

// Wait blocks until the child exits and is reaped, then returns the shared
// exec.Cmd.Wait result. Multiple goroutines may call Wait concurrently. Wait
// returns ErrExecCmdNotStarted when called before a successful Start.
func (c *ExecCmd) Wait() error {
	c.mu.RLock()
	if !c.started || c.cmd == nil || c.cmd.Process == nil {
		c.mu.RUnlock()
		return ErrExecCmdNotStarted
	}
	c.mu.RUnlock()
	<-c.doneChan
	return c.waitErr

}

// Stop signals the process gracefully and waits for it to exit. If a positive
// grace period expires, Stop forces termination and waits for reaping. A zero
// grace period waits indefinitely after the graceful signal. Concurrent calls
// share the same stop operation and result.
//
// With WithProcessTreeTermination and a positive grace period, Stop returns
// only once the whole tree is gone: descendants still running when the direct
// child exits get the remainder of the grace period, and are then killed. With
// a zero grace period Stop does not wait for descendants.
//
// Stop also stops the descendants of a command whose direct child has already
// exited, while that tree is still tracked (see TreeDone). They get the
// graceful signal and the grace period on Unix; on Windows, where no graceful
// signal can safely reach them once the direct child is gone, they are
// terminated at once. StopRequested is not set in that case: the command itself
// was not asked to stop.
//
// Stop is StopContext with context.Background.
func (c *ExecCmd) Stop() error {
	return c.StopContext(context.Background())
}

// StopContext is Stop bounded by ctx. If ctx ends before the stop completes,
// StopContext forcibly terminates the command, and its tree while that is
// tracked, then returns an error wrapping ctx.Err() without waiting for them
// to be reaped. A stop shared with concurrent callers is escalated for all of
// them; a caller still waiting receives the stop's final result.
func (c *ExecCmd) StopContext(ctx context.Context) error {
	abandoned, err := c.stop(ctx)
	if abandoned {
		return errors.Join(abandonedError("stop", ctx), err)
	}
	return err
}

// stop runs or joins the command's stop operation and waits for it or for ctx.
// When ctx ends first it kills the command, reports abandoned, and returns
// only the kill's error.
func (c *ExecCmd) stop(ctx context.Context) (abandoned bool, err error) {
	c.mu.Lock()
	if !c.started || c.cmd == nil || c.cmd.Process == nil {
		c.mu.Unlock()
		return false, ErrExecCmdNotStarted
	}
	// Checked before status: the direct child may already be reaped while a stop
	// is still waiting for the rest of its process tree, and a concurrent caller
	// must share that wait rather than return early.
	if !c.stopping {
		leaderRunning := c.status == ExecCmdStatusRunning
		if !leaderRunning && !c.treeTracked {
			c.mu.Unlock()
			return false, nil
		}
		// Set only once the command is known to be alive, so that stopping
		// something that has already died is not mistaken for having stopped it.
		if leaderRunning {
			c.stopRequested = true
		}
		c.stopping = true
		c.stopDone = make(chan struct{})
		// On its own goroutine, so that a caller whose context ends can leave it.
		go c.runStop(leaderRunning, c.doneChan, c.stopDone, c.gracePeriod)
	}
	stopDone := c.stopDone
	c.mu.Unlock()

	select {
	case <-stopDone:
		return false, c.stopResult()
	case <-ctx.Done():
	}
	select {
	case <-stopDone:
		return false, c.stopResult()
	default:
	}
	// The kill lets runStop's waits finish: the reaper sees the exit and the
	// tree drains. Only a Done held open by output-pipe holders that procman
	// cannot reach outlasts it, and runStop then waits on in the background.
	return true, c.kill()
}

func (c *ExecCmd) stopResult() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stopErr
}

// runStop performs one stop operation and publishes its result to every caller
// sharing it.
func (c *ExecCmd) runStop(leaderRunning bool, doneChan, stopDone chan struct{}, gracePeriod time.Duration) {
	var err error
	if leaderRunning {
		err = c.stopRunning(doneChan, gracePeriod)
	} else {
		err = c.stopTrackedTree(gracePeriod)
	}
	c.mu.Lock()
	c.stopErr = err
	c.stopping = false
	close(stopDone)
	c.mu.Unlock()
}

// stopRunning stops a command whose direct child is running: graceful signal,
// grace period, then forced termination.
func (c *ExecCmd) stopRunning(doneChan chan struct{}, gracePeriod time.Duration) error {
	signalErr := gracefulSignal(c)
	if signalErr != nil {
		killErr := forceKill(c)
		if killErr != nil {
			return errors.Join(signalErr, killErr)
		}
		<-doneChan
		return signalErr
	}
	if gracePeriod <= 0 {
		<-doneChan
		return nil
	}

	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	select {
	case <-doneChan:
	case <-timer.C:
		if err := forceKill(c); err != nil {
			return err
		}
		<-doneChan
		// Forced termination covered the whole tree; wait for it to drain.
		<-c.treeDone
		return nil
	}
	// The direct child exited within the grace period, but descendants may have
	// ignored the graceful signal. Give them what is left of it.
	return c.awaitTree(timer.C)
}

// stopTrackedTree stops what remains of a tree whose direct child has already
// been reaped, with the same escalation Stop applies to a running command.
func (c *ExecCmd) stopTrackedTree(gracePeriod time.Duration) error {
	if !gracefulTreeSignal {
		if err := c.signalTrackedTree(false); err != nil {
			return err
		}
		<-c.treeDone
		return nil
	}
	if err := c.signalTrackedTree(true); err != nil {
		return err
	}
	if gracePeriod <= 0 {
		return nil
	}
	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	return c.awaitTree(timer.C)
}

// awaitTree waits for TreeDone, killing what remains of the tree if deadline
// fires first.
func (c *ExecCmd) awaitTree(deadline <-chan time.Time) error {
	select {
	case <-c.treeDone:
		return nil
	case <-deadline:
		if err := c.signalTrackedTree(false); err != nil {
			return err
		}
		<-c.treeDone
		return nil
	}
}

// treePollInterval is how often a tracked tree is checked for remaining
// members. Neither Unix process groups nor Windows Job Objects offer an exit
// notification that does not also require being their parent.
const treePollInterval = 50 * time.Millisecond

// trackTree runs on the reaper goroutine once the direct child has been reaped
// with members left in its tree, and returns when the tree is first observed
// empty. An empty process group or Job Object cannot gain members again, so
// that observation is final.
func (c *ExecCmd) trackTree() {
	ticker := time.NewTicker(treePollInterval)
	defer ticker.Stop()
	for range ticker.C {
		c.mu.Lock()
		if c.treeTracked && treeAlive(c) {
			c.mu.Unlock()
			continue
		}
		c.treeTracked = false
		c.mu.Unlock()
		return
	}
}

// signalTrackedTree signals what remains of the tree after the direct child
// has been reaped. It acts only while the tree is tracked, and checks for
// members again immediately before signalling, under the same lock that ends
// tracking. The identifier it signals has therefore just been seen in use, and
// on Unix POSIX does not reuse a process-group ID while the group has members.
func (c *ExecCmd) signalTrackedTree(graceful bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.treeTracked {
		return nil
	}
	if !treeAlive(c) {
		c.treeTracked = false
		return nil
	}
	return signalTree(c, graceful)
}

// needsStop reports whether Stop has anything to reach: the direct child, or
// the tree it left behind while that is still tracked.
//
// A direct child that has died while Wait is still draining its output pipes
// keeps the running status until Wait returns. With process-tree termination
// the status decides, because Stop reaches the descendants holding those pipes
// through the tree. Without it nothing can reach them: both signals find the
// child already reaped and Stop would wait on Done for as long as they live,
// so the operating system's view decides instead.
func (c *ExecCmd) needsStop() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.started:
		return false
	case c.treeTracked:
		return true
	case c.status != ExecCmdStatusRunning:
		return false
	case c.processTreeTermination:
		return true
	default:
		return c.cmd != nil && c.cmd.Process != nil && processIsAlive(c.cmd.Process)
	}
}

// GetProcessState returns the child's process state after it has been reaped.
// It returns nil before Start and while the child is still running.
func (c *ExecCmd) GetProcessState() *os.ProcessState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil || !c.started {
		return nil
	}
	return c.cmd.ProcessState
}

// Done returns a channel closed once the command has exited and been reaped.
// Everything ExecCmd reports about the exit is settled before the close, so a
// receive is a safe point to read ExitCode or GetProcessState. It is safe to
// call before Start; the channel simply stays open.
func (c *ExecCmd) Done() <-chan struct{} {
	return c.doneChan
}

// TreeDone returns a channel closed once the command has been reaped and no
// process remains in its tree. It always closes after Done. With
// WithProcessTreeTermination the tree is the command's process group on Unix
// and its Job Object on Windows; descendants that left it are not counted.
// Without that option, or when nothing outlives the direct child, it closes
// immediately after Done.
//
// Between Done and TreeDone the leftover descendants are tracked: Stop,
// StopAll, KillAll and Shutdown can still reach them, and WithOrphanCleanup
// terminates them automatically. Tracking costs one check of the tree every
// 50ms until it empties.
func (c *ExecCmd) TreeDone() <-chan struct{} {
	return c.treeDone
}

// ExitCode returns the platform exit status reported by os.ProcessState. It
// returns -1 before the command has been reaped. On Unix, -1 also represents a
// command killed by a signal; use Done to distinguish that from a pending exit.
func (c *ExecCmd) ExitCode() int {
	state := c.GetProcessState()
	if state == nil {
		return -1
	}
	return state.ExitCode()
}

// StopRequested reports whether Stop or KillAll requested termination while
// this command was observed running. It stays true once set, so an exited
// command still says whether it was asked to go — the difference between a
// shutdown and a crash. It does not prove that the operating system delivered
// a signal, and it is false for termination requested outside this Procman.
func (c *ExecCmd) StopRequested() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stopRequested
}

// ExitedAt returns the time recorded after the command was reaped and platform
// cleanup completed, and the zero time before that. It is not the exact moment
// the kernel ended the process, which nothing here can observe.
func (c *ExecCmd) ExitedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.exitedAt
}

// StartedAt returns when the command was started, and the zero time before
// that. With ExitedAt it gives the command's lifetime.
func (c *ExecCmd) StartedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.startedAt
}
