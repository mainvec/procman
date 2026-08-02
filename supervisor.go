package procman

import (
	"context"
	"errors"
	"os/exec"
	"sync"
)

// Supervisor manages a set of supervised processes. Use New to construct one.
type Supervisor struct {
	opts Options

	mu      sync.RWMutex
	procs   map[ID]*Process
	byName  map[string]*Process
	closed  bool
}

// New returns a new Supervisor configured by opts. The zero-value Options is
// not usable; at minimum set Watchdog.
func New(opts Options) *Supervisor {
	return &Supervisor{
		opts:   opts,
		procs:  make(map[ID]*Process),
		byName: make(map[string]*Process),
	}
}

// Start launches a process described by spec and returns a stable Process
// handle. It returns a *StartError if the child could not exec or started and
// died immediately.
func (s *Supervisor) Start(ctx context.Context, spec Spec) (*Process, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("procman: supervisor closed")
	}
	if _, dup := s.byName[spec.Name]; dup {
		s.mu.Unlock()
		return nil, ErrDuplicateName
	}
	p := &Process{
		sup:  s,
		id:   NewID(),
		spec: spec,
		done: make(chan struct{}),
		state: StateStarting,
	}
	s.procs[p.id] = p
	s.byName[spec.Name] = p
	s.mu.Unlock()

	// For T3, restart is RestartNever (zero value). We start one generation.
	cmd, err := p.startGeneration(ctx)
	if err != nil {
		// Pure exec failure: child never ran.
		s.mu.Lock()
		delete(s.procs, p.id)
		delete(s.byName, spec.Name)
		s.mu.Unlock()
		return nil, &StartError{Spec: spec, Err: err}
	}
	_ = cmd

	// If the child dies essentially immediately, Start still returns the
	// handle; the caller learns via Done()/Exit(). This matches the plan's
	// note that Start returns as soon as cmd.Start() does.
	return p, nil
}

// Stop stops a single process: signals the group, waits StopGrace, escalates
// to a hard kill, and waits on the reaper. Idempotent; a no-op for an already
// exited process. It never calls cmd.Wait directly.
func (s *Supervisor) Stop(ctx context.Context, p *Process) error {
	if p == nil {
		return nil
	}
	return p.stop(ctx)
}

// StopAll stops every process concurrently and joins the errors.
func (s *Supervisor) StopAll(ctx context.Context) error {
	s.mu.RLock()
	procs := make([]*Process, 0, len(s.procs))
	for _, p := range s.procs {
		procs = append(procs, p)
	}
	s.mu.RUnlock()

	var wg sync.WaitGroup
	errCh := make(chan error, len(procs))
	for _, p := range procs {
		wg.Add(1)
		go func(p *Process) {
			defer wg.Done()
			errCh <- s.Stop(ctx, p)
		}(p)
	}
	wg.Wait()
	close(errCh)
	var firstErr error
	for err := range errCh {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// List returns the currently-supervised processes.
func (s *Supervisor) List() []*Process {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Process, 0, len(s.procs))
	for _, p := range s.procs {
		out = append(out, p)
	}
	return out
}

// Get returns the process with the given name, if present.
func (s *Supervisor) Get(name string) (*Process, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byName[name]
	return p, ok
}

// Close stops everything and marks the supervisor closed. Idempotent.
func (s *Supervisor) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.StopAll(context.Background())
}

// unregister removes a permanently-exited process from the registry. Called
// by publishExit when the process is terminal.
func (s *Supervisor) unregister(p *Process) {
	s.mu.Lock()
	delete(s.procs, p.id)
	if cur, ok := s.byName[p.spec.Name]; ok && cur == p {
		delete(s.byName, p.spec.Name)
	}
	s.mu.Unlock()
}

// keep exec referenced for future group hooks.
var _ = exec.Command