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
package procman
