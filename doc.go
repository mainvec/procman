// Package procman supervises child processes and coordinates graceful,
// concurrent shutdown across platforms.
//
// A Procman registers ExecCmd values, starts each command at most once, reaps
// every started child, and dispatches asynchronous start and exit callbacks.
// Stop and StopAll perform graceful shutdown with optional forced escalation.
// Shutdown additionally prevents new work and closes the event loop.
//
// Process-tree termination and cleanup after an unexpected parent exit are
// opt-in capabilities with platform-specific implementations. Use
// SupportsParentDeathCleanup before requiring native parent-death cleanup, or
// use WithParentDeathCleanupIfSupported for best-effort portable behavior.
//
// # Learning that a command has ended
//
// There are two ways, and they promise different things.
//
// The Procman.OnStart and Procman.OnExit fields receive lifecycle events from
// every command. They are dispatched through a single event loop over a
// buffered channel and are dropped when it fills, so a slow callback costs
// delivery rather than throughput. They are for logging, metrics and other
// observability.
//
// ExecCmd.Done, ExecCmd.ExitCode, WithOnStart and WithOnExit concern one
// command and cannot be dropped: the hooks are called directly from that
// command's own goroutine, and Done is closed only once the exit hook has
// returned. The cost lands on the caller instead — a hook that blocks delays
// that command's Done, Wait and Shutdown, and an exit hook that panics ends the
// process, because it runs on the reaper goroutine where nothing can recover
// for it. An exit hook must not call Wait or Shutdown, because both wait for the
// hook to return. Use these where missing an exit would be a bug.
package procman
