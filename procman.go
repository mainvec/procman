package procman

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"sync"
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
)

// ExecCmdOption configures a command managed by Procman.
type ExecCmdOption func(*execCmdOptions) error

type execCmdOptions struct {
	restartPolicy          RestartPolicy
	gracePeriod            time.Duration
	processTreeTermination bool
	parentDeathCleanup     bool
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

// WithRestartPolicy selects the intended restart policy for a command.
// Restart execution is not implemented yet.
func WithRestartPolicy(p RestartPolicy) ExecCmdOption {
	return func(o *execCmdOptions) error {
		// validate
		if p < RestartPolicyNever || p > RestartPolicyOnFailure {
			return errors.New("invalid restart policy")
		}
		o.restartPolicy = p
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
}

type event struct {
	kind int // 0=start, 1=exit
	ecmd *ExecCmd
	err  error
}

// RestartPolicy selects when a command should be restarted after exit.
// Restart execution is not implemented yet.
type RestartPolicy int

const (
	// RestartPolicyNever disables automatic restart.
	RestartPolicyNever RestartPolicy = iota
	// RestartPolicyAlways requests restart after every exit.
	RestartPolicyAlways
	// RestartPolicyOnFailure requests restart only after unsuccessful exit.
	RestartPolicyOnFailure
)

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
	restartPolicy          RestartPolicy
	gracePeriod            time.Duration
	processTreeTermination bool
	parentDeathCleanup     bool
	// onStart and onExit are read without holding mu: both are fixed at
	// construction and never reassigned.
	onStart  func(*ExecCmd)
	onExit   func(*ExecCmd, error)
	platform platformState
}

// NewProcman creates a running Procman with default command behavior.
func NewProcman() *Procman {
	return newProcman(execCmdOptions{restartPolicy: RestartPolicyNever})
}

// NewProcmanWithOptions creates a running Procman with validated construction
// options. It returns an error when a Procman option or default command option
// is invalid or unsupported.
func NewProcmanWithOptions(opts ...ProcmanOption) (*Procman, error) {
	o := procmanOptions{
		defaultExecCmdOptions: execCmdOptions{restartPolicy: RestartPolicyNever},
	}
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
			pm.logger.Warn("procman: notifyOnStart: dropping event for %s[%v] (channel full)", ecmd.Name, ecmd.ID())
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
			pm.logger.Warn("procman: notifyOnExit: dropping event for %s[%v] (channel full)", ecmd.Name, ecmd.ID())
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
	childId := NewID()
	child := &ExecCmd{
		Name:                   cmd.Args[0],
		Args:                   cmd.Args[1:],
		procman:                p,
		procmanId:              childId,
		cmd:                    cmd, // store now; Start() will use it
		status:                 ExecCmdStatusNotStarted,
		doneChan:               make(chan struct{}),
		restartPolicy:          o.restartPolicy,
		processTreeTermination: o.processTreeTermination,
		parentDeathCleanup:     o.parentDeathCleanup,
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

func (p *Procman) removeExecCmd(id ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.execCmds, id)
}

// ListExecCmdes returns a snapshot of all registered commands, including
// commands that have not started and commands that have exited.
func (p *Procman) ListExecCmdes() []*ExecCmd {
	p.mu.RLock()
	defer p.mu.RUnlock()
	children := make([]*ExecCmd, 0, len(p.execCmds))
	for _, child := range p.execCmds {
		children = append(children, child)
	}
	return children
}

// KillAll requests immediate, non-graceful termination of every started
// command. It ignores termination errors and does not wait for process exit.
func (p *Procman) KillAll() {
	p.mu.RLock()
	cmds := make([]*ExecCmd, 0, len(p.execCmds))
	for _, child := range p.execCmds {
		cmds = append(cmds, child)
	}
	p.mu.RUnlock()
	for _, child := range cmds {
		child.mu.Lock()
		started := child.started
		cmd := child.cmd
		if started && cmd != nil && cmd.Process != nil && child.status == ExecCmdStatusRunning {
			child.stopRequested = true
		}
		child.mu.Unlock()
		if started && cmd != nil && cmd.Process != nil {
			_ = forceKill(child)
		}
	}
}

// StopAll gracefully stops all currently running commands concurrently and
// waits for them to exit. It returns the joined errors from commands that fail
// to stop. Registered commands that have not started are ignored.
func (p *Procman) StopAll() error {
	cmds := p.ListExecCmdes()
	errCh := make(chan error, len(cmds))
	var waitGroup sync.WaitGroup

	for _, cmd := range cmds {
		if !cmd.IsRunning() {
			continue
		}

		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if err := cmd.Stop(); err != nil {
				errCh <- err
			}
		}()
	}

	waitGroup.Wait()
	close(errCh)
	errs := make([]error, 0, len(errCh))
	for err := range errCh {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Shutdown prevents new commands from being created or started, stops running
// commands, waits for started commands to be reaped, drains callbacks, and
// stops the event loop. Concurrent and repeated calls return the same result.
func (p *Procman) Shutdown() error {
	p.shutdownOnce.Do(func() {
		p.mu.Lock()
		p.status = ProcmanStatusShutdown
		p.mu.Unlock()

		p.shutdownErr = p.StopAll()
		for _, cmd := range p.ListExecCmdes() {
			if cmd.IsStarted() {
				<-cmd.doneChan
			}
		}
		p.WaitEventLoop()
		p.StopEventLoop()
	})
	return p.shutdownErr
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
	cmd := c.cmd
	if err := prepareExecCmd(c); err != nil {
		c.status = ExecCmdStatusFailed
		c.mu.Unlock()
		return err
	}
	err := cmd.Start()
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
		cleanupExecCmd(c)
		c.mu.Lock()
		if err != nil {
			c.status = ExecCmdStatusFailed
			c.waitErr = err
		} else {
			c.status = ExecCmdStatusExited
		}
		c.exitedAt = time.Now()
		c.mu.Unlock()
		c.procman.notifyOnExit(c, err)
		// Before the close, so that anything waiting on Done sees a command the
		// hook has already finished with.
		if c.onExit != nil {
			c.onExit(c, err)
		}
		close(c.doneChan)
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
func (c *ExecCmd) Stop() error {
	c.mu.Lock()
	if !c.started || c.cmd == nil || c.cmd.Process == nil {
		c.mu.Unlock()
		return ErrExecCmdNotStarted
	}
	if c.status != ExecCmdStatusRunning {
		c.mu.Unlock()
		return nil
	}
	// Set only once the command is known to be alive, so that stopping something
	// that has already died is not mistaken for having stopped it.
	c.stopRequested = true
	doneChan := c.doneChan
	if c.stopping {
		stopDone := c.stopDone
		c.mu.Unlock()
		<-stopDone
		c.mu.RLock()
		err := c.stopErr
		c.mu.RUnlock()
		return err
	}
	c.stopping = true
	c.stopDone = make(chan struct{})
	stopDone := c.stopDone
	gracePeriod := c.gracePeriod
	c.mu.Unlock()
	complete := func(err error) error {
		c.mu.Lock()
		c.stopErr = err
		c.stopping = false
		close(stopDone)
		c.mu.Unlock()
		return err
	}

	signalErr := gracefulSignal(c)
	if signalErr != nil {
		killErr := forceKill(c)
		if killErr != nil {
			return complete(errors.Join(signalErr, killErr))
		}
		<-doneChan
		return complete(signalErr)
	}
	if gracePeriod <= 0 {
		<-doneChan
		return complete(nil)
	}

	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	select {
	case <-doneChan:
		return complete(nil)
	case <-timer.C:
		if err := forceKill(c); err != nil {
			return complete(err)
		}
		<-doneChan
		return complete(nil)
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
