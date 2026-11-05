//go:build linux

package procman

import (
	"runtime"
	"sync"
	"syscall"
)

// SupportsParentDeathCleanup reports whether native parent-death cleanup is
// available on the current platform.
func SupportsParentDeathCleanup() bool {
	return true
}

func configureParentDeathSignal(child *ExecCmd) error {
	child.cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	return nil
}

// startProcess starts the child. With parent-death cleanup it does so from
// spawnThread, because the kernel delivers Pdeathsig when the thread that
// forked the child exits, not when the process does. Go terminates an OS
// thread whenever a goroutine locked to it exits, and any thread may have run
// an earlier Start, so without this any code in the program that locks a
// goroutine and lets it finish can kill unrelated children.
func startProcess(child *ExecCmd) error {
	if !child.parentDeathCleanup {
		return child.cmd.Start()
	}
	return spawnThread.run(child.cmd.Start)
}

// spawnThread is the one OS thread that forks children with Pdeathsig.
var spawnThread lockedThread

// lockedThread runs functions on a single goroutine that is locked to its OS
// thread and never returns. Go terminates a locked thread only when its
// goroutine exits, and runs no other goroutine on it, so the thread lives as
// long as the process. It is started on first use; programs that never ask
// for parent-death cleanup never create it. Calls are serialised.
type lockedThread struct {
	once sync.Once
	reqs chan func()
}

func (t *lockedThread) run(fn func() error) error {
	t.once.Do(func() {
		t.reqs = make(chan func())
		go t.loop()
	})
	var (
		err      error
		panicked any
		done     = make(chan struct{})
	)
	t.reqs <- func() {
		defer close(done)
		defer func() { panicked = recover() }()
		err = fn()
	}
	<-done
	// Re-raised on the caller's goroutine, where it can be recovered, rather
	// than ending the process from the locked thread.
	if panicked != nil {
		panic(panicked)
	}
	return err
}

func (t *lockedThread) loop() {
	runtime.LockOSThread()
	for fn := range t.reqs {
		fn()
	}
}
