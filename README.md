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
	when its creating parent thread exits. Descendants are not covered.
- Windows uses a kill-on-close Job Object, which terminates the process tree.
- macOS and other non-Linux Unix platforms are currently unsupported.

Linux ties `Pdeathsig` to the creating OS thread rather than the whole parent
process. It handles abrupt process exit because all threads terminate, but the
Go runtime may move goroutines or retire OS threads. It is therefore not as
strong a process-lifetime guarantee as a Windows Job Object.

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

Available state methods:

- `ID()` returns the command's procman identifier.
- `Pid()` returns the child PID, or `0` before start.
- `IsStarted()` reports whether `Start` succeeded.
- `IsRunning()` checks whether the process is still running.
- `IsExited()` reports a successful or unsuccessful exit.
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

Calling `Stop` before `Start` returns `procman.ErrExecCmdNotStarted`. Repeated
concurrent calls wait for the same process exit.

Stop every currently running command concurrently:

```go
if err := pm.StopAll(); err != nil {
	log.Printf("one or more processes failed to stop: %v", err)
}
```

`KillAll` is the immediate, non-graceful alternative.

Use `Shutdown` when the process manager itself is no longer needed:

```go
if err := pm.Shutdown(); err != nil {
	log.Printf("shutdown failed: %v", err)
}
```

`Shutdown` rejects new commands, prevents registered unstarted commands from
starting, stops running commands, drains lifecycle callbacks, and stops the
event loop. It is safe to call concurrently or more than once. Command creation
and start attempts after shutdown return `procman.ErrProcmanShutdown`.

## Lifecycle callbacks

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

## Registry

Every command created by a `Procman` is registered immediately, including
commands that have not started yet.

```go
cmd, ok := pm.GetExecCmd(id)
cmds := pm.ListExecCmdes()
```

`StopAll` ignores commands that are not running.

## Platform behavior

- Linux: `WithProcessTreeTermination` signals the process group during explicit
  shutdown. `WithParentDeathCleanup` configures `Pdeathsig=SIGKILL` so the
  direct child is killed if the procman-owning application dies unexpectedly.
- macOS and other non-Linux Unix: `WithProcessTreeTermination` signals the
  process group. Parent-death cleanup is unsupported.
- Windows console processes: graceful stop sends `CTRL_BREAK_EVENT` to the
	child's process group. Process-tree termination and parent-death cleanup use
	a Job Object; parent-death cleanup enables kill-on-close so the process tree
	is terminated if the procman-owning application exits unexpectedly.
- Forced shutdown uses the platform's immediate process-kill operation.

Windows graceful shutdown requires a console process that handles
`CTRL_BREAK_EVENT`. GUI applications, Windows services, and detached processes
need application-specific shutdown mechanisms.

## Current limitations

- Parent-death cleanup is opt-in and unsupported on non-Linux Unix platforms.
- Linux parent-death cleanup covers only the direct child, not descendants.
- Linux `Pdeathsig` follows OS-thread lifetime, not Go process lifetime.
- Descendants that deliberately leave their process group or Windows Job Object
	are outside procman's control.
- Restart policy options exist, but restart behavior is not implemented yet.

## TODO

- Add an optional Unix watchdog for cleanup after unexpected parent exit.
- Implement restart policies and backoff.
