# procman

Cross-platform process lifecycle management for Go.

`procman` starts and reaps child processes, supports graceful shutdown with
forced-kill escalation, and can stop multiple processes concurrently.

## Install

```sh
go get github.com/mainvec/procman
```

## Quick start

```go
package main

import (
	"log"
	"time"

	"github.com/mainvec/procman"
)

func main() {
	pm := procman.NewProcman()

	cmd, err := pm.NewExecCmd(
		"my-server",
		procman.Args("--port", "8080"),
		procman.WithGracePeriod(5*time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	log.Printf("started pid %d", cmd.Pid())

	// Stop waits for graceful exit and force-kills after five seconds.
	if err := cmd.Stop(); err != nil {
		log.Printf("stop failed: %v", err)
	}
}
```

Each `ExecCmd` can be started once. `Start` returns an error if the executable
cannot be launched or if the command was already started.

## Creating commands

Create a command from a name and argument slice:

```go
cmd, err := pm.NewExecCmd("worker", procman.Args("--queue", "emails"))
```

Options follow the functional-options pattern:

```go
cmd, err := pm.NewExecCmd(
	"worker",
	procman.Args("--queue", "emails"),
	procman.WithGracePeriod(10*time.Second),
	procman.WithProcessTreeTermination(),
)
```

Set defaults once when most commands share the same behavior:

```go
pm, err := procman.NewProcmanWithOptions(
	procman.WithDefaultExecCmdOptions(
		procman.WithGracePeriod(10*time.Second),
		procman.WithProcessTreeTermination(),
		procman.WithParentDeathCleanupIfSupported(),
	),
)
if err != nil {
	log.Fatal(err)
}

cmd, err := pm.NewExecCmd("worker", procman.Args("--queue", "emails"))
```

Default options are validated when the Procman is created and inherited by
commands created through either `NewExecCmd` or `NewExecCmdFromCmd`.
Command-specific options are applied afterward, so scalar settings such as the
grace period can override their defaults. Enable-only options such as
`WithProcessTreeTermination` remain enabled when inherited.

`WithProcessTreeTermination` makes `Stop`, `StopAll`, `KillAll`, and `Shutdown`
target the command's process tree. Without it, explicit lifecycle operations
manage only the direct child.

Parent-death cleanup handles cases where the application running procman exits
without calling `Stop` or `Shutdown`. This can happen when the application
crashes, receives `SIGKILL`, is force-terminated by the operating system, or
otherwise exits before it can clean up its child processes.

It is a separate capability from process-tree termination:

```go
cmd, err = pm.NewExecCmd(
	"worker",
	procman.Args("--queue", "emails"),
	procman.WithParentDeathCleanupIfSupported(),
)
```

`WithParentDeathCleanupIfSupported` enables cleanup where available and does
nothing on unsupported platforms. Use `SupportsParentDeathCleanup` when the
application needs to report whether cleanup is active. Use the strict
`WithParentDeathCleanup` option when lack of support must fail command creation.

`WithParentDeathCleanup` asks the operating system to terminate managed child
processes when the procman-owning application dies unexpectedly. It is not
needed for normal graceful shutdown, where `Stop` or `Shutdown` performs the
cleanup. The option returns `procman.ErrParentDeathCleanupUnsupported` on
unsupported platforms. Its behavior is platform-specific:

- Linux configures `Pdeathsig=SIGKILL`, so the kernel kills the direct child
	when the procman-owning process dies. Descendants are not covered.
- Windows uses a kill-on-close Job Object, which terminates the process tree.
- macOS and other non-Linux Unix platforms are currently unsupported.

The kernel delivers `Pdeathsig` when the *thread* that forked the child exits,
not when the process does, and Go terminates an OS thread whenever a goroutine
locked to it returns. Procman therefore starts these commands from one
dedicated OS thread that stays locked for the life of the process, so the
signal fires only when the procman-owning process itself dies. That thread is
created on first use, and starts that use it are serialised.

To configure environment variables, working directory, standard streams, or
other `os/exec` fields, create an `*exec.Cmd` first:

```go
nativeCmd := exec.Command("worker", "--queue", "emails")
nativeCmd.Dir = "/srv/app"
nativeCmd.Env = append(os.Environ(), "APP_ENV=production")
nativeCmd.Stdout = os.Stdout
nativeCmd.Stderr = os.Stderr

cmd, err := pm.NewExecCmdFromCmd(nativeCmd,
	procman.WithGracePeriod(10*time.Second),
)
```

The supplied command must not already be started. Procman executes that exact
`*exec.Cmd` without rebuilding it.

When `Stdout` or `Stderr` is a writer rather than an `*os.File`, `os/exec`
copies the output through a pipe, and `Wait` (and with it `Done`) returns only
once every process holding that pipe has closed it, descendants included. A
descendant that outlives the command then delays `Done` for as long as it runs.
Set `exec.Cmd.WaitDelay` to bound that wait, and use process-tree termination
so procman can still reach the descendant afterwards (see
[Descendants that outlive the command](#descendants-that-outlive-the-command)).
Do not use `StdoutPipe` or `StderrPipe`: the reaper calls `Wait` as soon as the
child exits, which closes those pipes under whatever is reading them.

## Waiting and exit status

`Wait` blocks until the process has exited and been reaped, then returns the
error from `exec.Cmd.Wait`:

```go
if err := cmd.Wait(); err != nil {
	log.Printf("process exited unsuccessfully: %v", err)
}
```

Multiple goroutines may call `Wait` on the same command. All callers receive
the same result. Calling `Wait` before `Start` returns
`procman.ErrExecCmdNotStarted`.

Use `Done` when exit needs to compose with other channel operations without a
dedicated goroutine:

```go
select {
case <-cmd.Done():
	log.Printf("process exited with status %d", cmd.ExitCode())
case <-ctx.Done():
	return ctx.Err()
}
```

`Done` closes after the child has been reaped and its per-command exit hook has
returned. `ExitCode` then matches `os.ProcessState.ExitCode`. Before reaping it
returns `-1`. On Unix, `-1` also represents termination by a signal; Windows
reports the termination status supplied by the operating system.

`StopRequested` separates a shutdown from a crash. It is true once `Stop` or
`KillAll` requests termination while a command is observed running, and stays
true after exit, so a supervisor can tell an exit it asked for from one it did
not:

```go
<-cmd.Done()
if !cmd.StopRequested() {
	log.Printf("worker died on its own with status %d", cmd.ExitCode())
}
```

It records intent, not proof that the operating system delivered a signal. It
is false for termination requested outside this `Procman`, and false when the
command had already exited before `Stop` was called — tidying up after something
already dead is not the same as having stopped it.

Available state methods:

- `ID()` returns the command's procman identifier.
- `Pid()` returns the child PID, or `0` before start.
- `IsStarted()` reports whether `Start` succeeded.
- `IsRunning()` checks whether the process is still running.
- `IsExited()` reports a successful or unsuccessful exit.
- `Done()` returns a channel closed after the process is reaped.
- `TreeDone()` returns a channel closed once no process remains in the
	command's tree; see below.
- `ExitCode()` returns the platform process exit status, or `-1` before reaping.
- `StopRequested()` reports whether this `Procman` asked the command to stop.
- `StartedAt()` returns when the command started, or the zero time before that.
- `ExitedAt()` returns when reaping and platform cleanup completed, or the zero
	time before that.
- `GetProcessState()` exposes `*os.ProcessState` after the process is reaped.

## Stopping processes

`Stop` is synchronous: it returns after the process exits.

```go
cmd, err := pm.NewExecCmd(
	"worker",
	procman.Args(),
	procman.WithGracePeriod(5*time.Second),
)

// Later: graceful signal, five-second wait, then forced kill if needed.
err = cmd.Stop()
```

If the grace period is greater than zero, procman force-kills a process that
does not exit before the period expires. With the default zero grace period,
procman sends the graceful signal and waits without a forced-kill timeout.

With `WithProcessTreeTermination` and a positive grace period, `Stop` returns
only once the whole tree is gone: descendants still running after the direct
child exits get the remainder of the grace period and are then killed with the
tree. With a zero grace period, `Stop` does not wait for descendants.

Calling `Stop` before `Start` returns `procman.ErrExecCmdNotStarted`. Repeated
concurrent calls wait for the same stop operation, including any wait for the
process tree.

Stop every currently running command concurrently:

```go
if err := pm.StopAll(); err != nil {
	log.Printf("one or more processes failed to stop: %v", err)
}
```

`KillAll` is the immediate, non-graceful alternative. It reaches running
commands and the tracked descendants of commands that have already exited.

Use `Shutdown` when the process manager itself is no longer needed:

```go
if err := pm.Shutdown(); err != nil {
	log.Printf("shutdown failed: %v", err)
}
```

`Shutdown` rejects new commands, prevents registered unstarted commands from
starting, stops running commands and tracked descendants, drains lifecycle
callbacks, and stops the event loop. It is safe to call concurrently or more than once. Command creation
and start attempts after shutdown return `procman.ErrProcmanShutdown`.

### Descendants that outlive the command

With `WithProcessTreeTermination`, a direct child can exit — crash, or finish —
while processes it started keep running in its tree: its process group on
Unix, its Job Object on Windows. Procman keeps tracking that tree after the
direct child is reaped, until it is first observed empty:

```go
<-cmd.Done()     // the direct child has been reaped
<-cmd.TreeDone() // and nothing is left in its tree
```

While a tree is tracked, `Stop`, `StopAll`, `KillAll`, and `Shutdown` reach the
remaining descendants. Without further configuration they are otherwise left
running. `WithOrphanCleanup` stops them as soon as the direct child exits on its
own, with the same graceful signal, grace period and forced termination as
`Stop`:

```go
cmd, err := pm.NewExecCmd(
	"worker",
	procman.Args("--queue", "emails"),
	procman.WithProcessTreeTermination(),
	procman.WithOrphanCleanup(),
	procman.WithGracePeriod(10*time.Second),
)
```

`WithOrphanCleanup` requires `WithProcessTreeTermination`. It does not act when
the exit was requested through `Stop` or `KillAll`, which handle the tree
themselves, and it does not set `StopRequested`: the command still exited on its
own.

Once a tree has been observed empty, procman never signals it again, because on
Unix its process-group ID may then be reused by an unrelated process group. Each
signal to a tracked tree is preceded by a fresh check that it still has members,
under the same lock that ends tracking. Tracking costs one check every 50ms per
tree with leftovers, until the tree empties.

On Windows, descendants left after the direct child exits receive no graceful
signal: `CTRL_BREAK_EVENT` addresses a process group by its leader's PID, which
Windows may already have reused. `Stop` terminates them through the Job Object
at once.

## Lifecycle callbacks

Procman-wide callbacks are asynchronous observability hooks suitable for logs,
metrics, and dashboards:

Callbacks are dispatched asynchronously so process supervision is not blocked:

```go
pm.OnStart = func(cmd *procman.ExecCmd) {
	log.Printf("started %s[%s]: pid=%d", cmd.Name, cmd.ID(), cmd.Pid())
}

pm.OnExit = func(cmd *procman.ExecCmd, err error) {
	log.Printf("exited %s[%s]: %v", cmd.Name, cmd.ID(), err)
}
```

Set callbacks before starting commands. Events are processed serially through a
buffered queue; events may be dropped if the queue is full. Use
`WaitEventLoop()` when shutdown or tests must wait for queued callbacks to
finish. Callbacks should return promptly.

Use command options when delivery must not be dropped:

```go
cmd, err := pm.NewExecCmd(
	"worker",
	procman.Args("--queue", "emails"),
	procman.WithOnStart(func(cmd *procman.ExecCmd) {
		log.Printf("started pid %d", cmd.Pid())
	}),
	procman.WithOnExit(func(cmd *procman.ExecCmd, err error) {
		log.Printf("exited with status %d: %v", cmd.ExitCode(), err)
	}),
)
```

`WithOnStart` runs synchronously before `Start` returns, after the reaper is
running and without a Procman lock held. `WithOnExit` runs on the reaper
goroutine after exit state is recorded and before `Done` closes. It must return
promptly: blocking delays that command's `Done`, `Wait`, and `Shutdown`, and it
must not call `Wait` or `Shutdown` itself because both wait for the hook to
return. A panic in an exit hook terminates the application.

## Registry

Every command created by a `Procman` is registered immediately, including
commands that have not started yet.

```go
cmd, ok := pm.GetExecCmd(id)
cmds := pm.ListExecCmdes()
```

`StopAll` ignores commands that have not started, and commands that have exited
with nothing left in their tree.

Finished commands stay registered, with their `exec.Cmd` and any output buffers
it references, until they are removed. A long-running supervisor should remove
them, either explicitly or automatically:

```go
// Explicitly, once the command has finished:
<-cmd.TreeDone()
err := pm.Remove(cmd.ID())

// Or automatically, for one command or as a manager default:
cmd, err := pm.NewExecCmd("worker", nil, procman.WithAutoRemove())
pm, err := procman.NewProcmanWithOptions(
	procman.WithDefaultExecCmdOptions(procman.WithAutoRemove()),
)
```

A command has finished once `TreeDone` has closed. Until then `Remove` returns
`procman.ErrExecCmdActive`, because `StopAll`, `KillAll`, and `Shutdown` find
commands through the registry. `WithAutoRemove` removes the command just before
`TreeDone` closes, after any `WithOnExit` callback. Asynchronous
`Procman.OnExit` callbacks may run after removal.

A command that is removed before it starts cannot be started: `Start` returns
`procman.ErrExecCmdRemoved`. Existing references to a removed `*ExecCmd` keep
working for reading its state and waiting on its channels.

## Platform behavior

- Linux: `WithProcessTreeTermination` signals the process group during explicit
  shutdown. `WithParentDeathCleanup` configures `Pdeathsig=SIGKILL` so the
  direct child is killed if the procman-owning application dies unexpectedly.
- macOS and other non-Linux Unix: `WithProcessTreeTermination` signals the
  process group. Parent-death cleanup is unsupported.
- Windows console processes: graceful stop sends `CTRL_BREAK_EVENT` to the
	child's process group. Process-tree termination and parent-death cleanup use
	a Job Object; parent-death cleanup enables kill-on-close so the process tree
	is terminated if the procman-owning application exits unexpectedly. With
	process-tree termination the Job Object stays open while descendants outlive
	the direct child, so they remain reachable; with parent-death cleanup alone it
	is closed when the direct child is reaped, which kills any descendants.
- Forced shutdown uses the platform's immediate process-kill operation.

Windows graceful shutdown requires a console process that handles
`CTRL_BREAK_EVENT`. GUI applications, Windows services, and detached processes
need application-specific shutdown mechanisms.

## Current limitations

Stopping and waiting:

- `Stop`, `StopAll` and `Shutdown` take no timeout or context. With the default
  zero grace period, a process that ignores the graceful signal keeps them
  blocked; set `WithGracePeriod` to bound the wait.
- The graceful signal is fixed: `SIGTERM` on Unix and `CTRL_BREAK_EVENT` on
  Windows. Windows console processes are the only Windows processes that receive
  it (see [Platform behavior](#platform-behavior)).
- When `Stdout`/`Stderr` are writers, `Done` waits for every holder of the
  output pipes. Without process-tree termination, procman cannot reach holders
  that outlive the direct child; set `exec.Cmd.WaitDelay` to bound the wait.

Descendants:

- Without `WithProcessTreeTermination`, only the direct child is signalled.
  Its descendants are neither stopped nor tracked.
- With it, procman reaches only the command's process group (Unix) or Job
  Object (Windows). A descendant that leaves it, by `setsid`, `setpgid` or
  Job Object breakaway, is not stopped and not counted by `TreeDone`.
- On Windows, a child can start processes between its creation and its
  assignment to the Job Object; those escape the job.

Parent-death cleanup:

- It is opt-in, and unsupported on macOS and other non-Linux Unix platforms.
- On Linux it kills only the direct child. Its descendants keep running.

## TODO

- Add an optional Unix watchdog for cleanup after unexpected parent exit ([#27](https://github.com/mainvec/procman/issues/27)).
