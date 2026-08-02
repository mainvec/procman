# procman

A zero-dependency, cross-platform Go library for robust process supervision.
`procman` closes the three gaps that `os/exec` leaves open:

1. **Children outlive their parent.** `cmd.Process.Kill()` signals one PID; a
   child that spawned its own children leaves them running. If the parent is
   `SIGKILL`ed, panics, or the host is restarted, nothing kills the child.
2. **Exit is not observed.** `Wait` is often never called on the happy path,
   leaving zombies; calling `Wait` from two places races with `ECHILD`.
3. **Output is unrecoverable.** `cmd.Stdout = os.Stderr` loses the startup
   diagnostic; hand-rolled `bufio.Scanner` pumps die silently on lines over
   64 KiB.

`procman` guarantees, on Linux, macOS and Windows, using nothing outside the
standard library:

- A child process, and its descendants, cannot survive the death of the
  parent — including `SIGKILL` of the parent.
- Every child is reaped exactly once, and its exit code and signal are
  available without polling.
- Child output is captured in one standardised way: streamed to writers,
  delivered as lines to a callback, and retained in a bounded in-memory tail.
- Optional restart with backoff and crash-loop detection.

## Two invariants

Everything in the package exists to hold one of two lines:

1. **A child never outlives its parent.** Enforced by the kernel where
   possible (Windows Job Object), by a dedicated watchdog process where not
   (Unix).
2. **The parent always learns that a child died.** Enforced by exactly one
   reaper goroutine per process, which owns `cmd.Wait()`.

## Platform table

| | Group isolation | Graceful stop | Hard kill | Parent-death guarantee |
|---|---|---|---|---|
| Linux | `Setpgid` | `kill(-pgid, SIGTERM)` | `kill(-pgid, SIGKILL)` | `/bin/sh` sidecar watchdog |
| macOS | `Setpgid` | `kill(-pgid, SIGTERM)` | `kill(-pgid, SIGKILL)` | `/bin/sh` sidecar watchdog |
| Windows | Job Object | `TerminateJobObject` | `TerminateJobObject` | `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` |

**Windows needs no watchdog.** The parent creates a Job Object with
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, assigns the child to it, and holds the
only handle. When the parent dies by any means, every handle closes and the
kernel terminates the whole job. This is stronger than anything available on
Unix and costs one handle.

**Unix needs a watchdog process**, because no kernel mechanism works. It is
spawned *beside* the target, not between the parent and the target:

```
parent ──▶ target      (started directly: normal stdio, normal Wait, real exit code)
       ──▶ watchdog    (holds the sentinel pipe on fd 3, knows the target's pgid)
```

The watchdog is `/bin/sh`, present on every supported system, running a fixed
three-step script:

```sh
read pgid <&3 || exit 0        # parent died before arming; nothing exists to kill
read done <&3 && exit 0        # parent stood us down; the target exited normally
kill -TERM -"$pgid" 2>/dev/null
sleep $grace
kill -KILL -"$pgid" 2>/dev/null
```

Spawn ordering is watchdog → target → write pgid, which makes the unprotected
window as small as it can be (the pgid does not exist until the target does).
No user-controlled data is interpolated into the script — the pgid arrives
over fd 3 and is validated as an integer before use.

**Fallback.** `FROM scratch` and distroless images have no `/bin/sh`. There the
parent falls back to re-executing itself: `os.Executable()` plus
`PROCMAN_WATCHDOG=1`, intercepted by a package `init()` that runs the same
three-step loop in Go and never returns. The exported `RunWatchdogAndExit()`
lets a caller invoke it explicitly as the first statement of `main` instead.
`Options.ShellPath` overrides the shell; `WatchdogOff` disables the guarantee.

## Quick start

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/mainvec/procman"
)

func main() {
	s := procman.New(procman.Options{Watchdog: procman.WatchdogAuto})
	defer s.Close()

	p, err := s.Start(context.Background(), procman.Spec{
		Name:      "server",
		Path:      "/usr/local/bin/myserver",
		Args:      []string{"--port", "8080"},
		StopGrace: 5 * time.Second,
		Restart: procman.RestartPolicy{
			Mode:         procman.RestartOnFailure,
			InitialDelay: 100 * time.Millisecond,
			MaxDelay:     10 * time.Second,
			Multiplier:   2.0,
			MaxRetries:   5,
			ResetAfter:   time.Minute,
		},
		LogTailLines: 200,
	})
	if err != nil {
		log.Fatalf("start: %v", err)
	}

	// Run until the process exits for good.
	<-p.Done()
	info, _ := p.Exit()
	log.Printf("server exited: code=%d signal=%q err=%v", info.Code, info.Signal, info.Err)
	for _, line := range p.LogTail() {
		log.Printf("  %s: %s", line.Stream, line.Text)
	}
}
```

## API

```go
type Supervisor struct{ /* ... */ }

func New(Options) *Supervisor
func (s *Supervisor) Start(ctx context.Context, spec Spec) (*Process, error)
func (s *Supervisor) Stop(ctx context.Context, p *Process) error
func (s *Supervisor) StopAll(ctx context.Context) error
func (s *Supervisor) List() []*Process
func (s *Supervisor) Get(name string) (*Process, bool)
func (s *Supervisor) Close() error
```

`Process` is a **stable handle**. A restart changes `PID()`, `StartedAt()` and
`Generation()` but not the handle, and `Done()` stays open. `Done()` closing
means *permanently* stopped: the policy is `RestartNever`, the caller called
`Stop`, or the retry budget is exhausted. Per-generation exits are delivered
through `Options.OnExit`, which fires once per generation **before** `Done()`
closes.

`Stop` sends a graceful signal to the whole group, waits `StopGrace`,
escalates to a hard kill if needed, and waits on the reaper (never on
`cmd.Wait` directly). It returns `ErrStopEscalated` when a hard kill was
required. It is idempotent and a no-op for an already-exited process.

## Limitations

These are documented rather than hidden:

- The Unix watchdog can be killed independently by PID; it is a process, not
  a kernel guarantee.
- A supervised child is a process-group leader (`Setpgid`), so a direct
  `setsid()` from it fails with `EPERM` and it cannot escape. A *grandchild*
  that calls `setsid()` is not a leader and escapes its process group on Unix;
  `procman` does not track descendants that detach.
- `Options.Watchdog` set to `WatchdogOff` opts out of the parent-death
  guarantee entirely.
- On Windows there is a brief unprotected window between `cmd.Start` and
  `AssignProcessToJobObject`; Go exposes no `CREATE_SUSPENDED` path.
- `stdout` and `stderr` are not ordered relative to each other; this is
  inherent to two pipes.
- On a system with no `/bin/sh` the fallback re-executes the host binary and
  its `init()` hook runs before `main`, so a host with expensive package-level
  initialisation pays that cost once per spawn.

## Zero dependencies

`go mod graph` produces no output. The package uses only the Go standard
library, including build and lint tooling — no `tool` directives in `go.mod`.
A CI step asserts this mechanically.

## Status

Pre-1.0 (`v0.1.0` makes no compatibility promise). See
[plans/001-process-supervision-library.md](plans/001-process-supervision-library.md)
for the full design and decision log.