# #004: Observable exit — let a consumer react without reimplementing reaping

**Type**: feature
**GitHub Issue**: [#4](https://github.com/mainvec/procman/issues/4)
**Branch**: feat/4-observable-exit

## Progress

- [x] T1: Export `ExecCmd.Done()` and `ExecCmd.ExitCode()`
- [x] T2: Per-command `WithOnStart` / `WithOnExit` hooks
- [x] T3: Document which delivery path is for which purpose
- [x] T4: Report whether an exit was asked for
- [x] T5: Report when the command started and exited

## Problem / Goal

`procman` already knows everything a consumer needs about an exit: the reaper goroutine started by
`ExecCmd.Start` calls `cmd.Wait()`, records `status` and `waitErr`, notifies, and closes
`doneChan`. None of that is reachable from outside in a form correctness can depend on.

**`OnStart`/`OnExit` are droppable.** `notifyOnStart` and `notifyOnExit` do a non-blocking send
into a 256-entry channel and, on the `default:` branch, decrement the in-flight counter and log a
warning. The event is gone; `Shutdown` drains only what the loop accepted. That is a defensible
trade for observability, which is what the field comments say the callbacks are for. It makes them
unusable for anything that must not be missed.

**`doneChan` has no accessor.** The reaper closes it, `Shutdown` reads it, but a consumer cannot
`select` on a command's exit. It has to start its own goroutine on `Wait()` — a second waiter on a
command that already has one, distinguishable from the reaper only by who wins.

**Exit status has to be re-derived.** `GetProcessState()` exposes `*os.ProcessState`, so every
consumer writes the same `if state != nil { state.ExitCode() }` dance and has to account for its
platform-specific termination status.

The result is visible in the first consumer. `zirafa-core/core.ProcessManager` runs a `reap`
goroutine per process that calls `ecmd.Wait()`, reads `GetProcessState().ExitCode()`, and closes
its own `done` channel — a duplicate of procman's reaper, existing only because procman's is not
observable. See [mainvec/zirafa#19](https://github.com/mainvec/zirafa/issues/19).

Success looks like: a consumer can learn that a command exited, and with what status, without
starting a goroutine of its own and without an event it might not receive.

## Goals

- A command's exit is observable as a channel, so it composes with `select`.
- A command's exit status is available as an `int` without the caller handling `nil`.
- A consumer can attach a callback to one command that is guaranteed to run.
- No change to the manager-wide `OnStart`/`OnExit` fields or their delivery.

## Non-goals

- **Making the manager-wide callbacks undroppable.** They are documented as asynchronous
  observability. A consumer that needs guaranteed delivery should use the per-command hook. Worth
  noting that the total number of undelivered *exit* events is bounded by the number of registered
  commands — there is exactly one per `ExecCmd`, ever — so an overflow queue would be cheap. It is
  still a separate decision and a separate change.
- **Moving `OnStart`/`OnExit` from public fields to constructor options.** They are read from the
  event-loop goroutine with no synchronisation, so setting one after a command has started is a
  data race. Real, but a breaking change and unrelated to this issue.
- **Restart policies.** Still unimplemented, still out of scope.
- **Renaming `ListExecCmdes`.** Breaking, and it deserves its own issue.

## Proposed Design

### Two delivery paths, each honest about what it is

| Path | Delivery | Scope | For |
|---|---|---|---|
| `Procman.OnStart` / `OnExit` | asynchronous, may drop | every command | metrics, logging, dashboards |
| `Done()` / `ExitCode()` / `WithOnExit` | synchronous, never dropped | one command | supervision that must not miss an exit |

### `Done` and `ExitCode`

`Done()` returns the existing `doneChan` as a receive-only channel. Nothing else changes: the
reaper already closes it last, after `status` and `waitErr` are written and after the manager-wide
notify, so a reader that wakes on it sees settled state.

`ExitCode()` reports the status the reaper obtained, and -1 for one that has not exited. It returns
`os.ProcessState.ExitCode()` unchanged: Unix also uses -1 for a command killed by a signal, while
Windows reports the termination status supplied to the operating system.

### Per-command hooks

`WithOnStart(fn)` and `WithOnExit(fn)` are `ExecCmdOption`s, so they compose with the existing
options and can be inherited through `WithDefaultExecCmdOptions`.

They are called **from that command's own goroutine, not the shared event loop**:

- `WithOnStart` runs after `Start` has launched the reaper and released the manager lock, but before
  `Start` returns.
- `WithOnExit` runs in the reaper, after `status`/`waitErr` are written and immediately before
  `close(doneChan)`.

Direct calls rather than queued events is the whole point: a direct call cannot be dropped. The
cost transfers to the caller and has to be documented — a hook that blocks delays `Done()`,
`Wait()` and `Shutdown()` for that one command. That is strictly better than the alternative,
where a blocking manager-wide callback stalls delivery for every command.

A hook is user code and will call back into the library, so it must run with nothing held that the
library needs. `Start` holds `Procman.mu.RLock` for its whole body; the start hook therefore cannot
be called from inside it, or a hook that registers a command deadlocks against `NewExecCmd`'s write
lock. Splitting `Start` into a locked `start` plus the hook call also puts the hook after the reaper
exists, so a hook may wait on `Done()`.

Ordering with the manager-wide callbacks is deliberately not specified beyond "the per-command
hook runs before `doneChan` closes". The manager-wide ones are asynchronous; promising an order
between a direct call and a queued one would be a promise about scheduling.

## Affected Modules

- `procman.go` — option plumbing and hooks; `ExecCmd` gains `Done`, `ExitCode`, `StopRequested`,
  `StartedAt`, and `ExitedAt`.
- `observable_exit_test.go`, `stop_requested_test.go`, `lifecycle_times_test.go` — portable
  behavioral and concurrency coverage.
- `observable_exit_unix_test.go` — Unix signal exit-code semantics.
- `doc.go` and `README.md` — delivery guarantees, lifecycle metadata, and callback constraints.

## Tasks

### T1: Export `ExecCmd.Done()` and `ExecCmd.ExitCode()`

**Outcome**: `Done() <-chan struct{}` returns the channel the reaper closes. `ExitCode() int`
returns the platform exit status and -1 before the command has exited.

**Verification**: A test selects on `Done()` for a command that exits on its own and asserts it
closes; a second asserts `ExitCode()` matches the code the child exited with, and is -1 after the
command is killed.

**Notes.** Done. `Done()` returns `doneChan` unchanged; nothing in the reaper moved. `ExitCode()` is
a thin read over `GetProcessState()`, so it inherits the existing locking and the existing nil case
rather than adding a second source of truth.

The tests needed a child that exits with a chosen status, so `procman_test.go` gained a
`--procman-exit` mode alongside the existing sleep and argv0 modes.

On Unix, a caller cannot tell "not exited" from "killed by a signal" by the code alone — both are
-1. That is `os.ProcessState.ExitCode`'s own convention and `Done()` answers the question, so the
alternative (a second return value, or a sentinel of our own) would have bought ambiguity of a
different kind. Stated in the doc comment.

The -1-for-a-signal assertion is Unix-only and lives in `observable_exit_unix_test.go`; it compiled
for Windows but would have failed on one. What holds everywhere — that `ExitCode` reports whatever
`os.ProcessState` reports — is asserted in `TestExitCodeAgreesWithTheProcessState`.

### T2: Per-command `WithOnStart` / `WithOnExit` hooks

**Outcome**: Both options exist, are inherited through `WithDefaultExecCmdOptions`, and are called
synchronously from the command's own goroutine.

**Verification**: A test asserts the exit hook runs for every one of many commands exiting at once —
the case the buffered channel drops. A test asserts the hook has already run by the time `Done()`
is closed. A test asserts a hook that blocks delays only its own command.

**Notes.** Done. The hooks are stored on `ExecCmd` at construction and never reassigned, so they are
read without holding `c.mu` — worth stating, because it is the opposite of how the manager-wide
fields behave.

An exit hook cannot call `Wait` or `Shutdown`: both wait for `doneChan`, which closes only after
the hook returns. The GoDoc and README state this explicitly rather than presenting the hook as
generally reentrant. Start hooks are reentrant after launch because the reaper is already running
and no manager lock is held.

Overflowing the 256-entry channel would have meant starting more than 256 processes to prove a point
about independence. Blocking the manager-wide `OnExit` on a channel proves the same thing with 20:
every per-command hook runs while the shared loop is stuck on the first event. Cheaper and more
direct about what is being claimed.

`WithOnStart` runs inside `Start`, so the test can assert with a non-blocking receive that it has
already happened when `Start` returns. That is a stronger claim than a timeout would make, and it is
the claim the doc comment makes.

Mutation-tested:

| Mutation | Test | Failure |
|---|---|---|
| `close(c.doneChan)` moved ahead of the exit hook | `TestABlockingExitHookDelaysOnlyItsOwnCommand` | `the blocked command's Done() closed while its hook was still running`, 5 runs of 5 |
| exit hook dispatched from `eventLoop` instead of the reaper | `TestPerCommandHooksDoNotDependOnTheEventLoop` | `0 of 20 exit hooks ran while the event loop was blocked` |
| start hook called inside the locked `start` | `TestWithOnStartMayCallBackIntoTheManager` | 40s timeout; the dump shows `NewExecCmdFromCmd` blocked on `Procman.mu.Lock` under `start` |

`TestWithOnExitRunsBeforeDoneCloses` survived the first mutation — the window between the close and
the hook is too small to catch by racing it. It documents the contract; the blocking test is what
enforces it.

### T3: Document which delivery path is for which purpose

**Outcome**: `doc.go` and the option comments state that the manager-wide callbacks are
observability and may be dropped, that the per-command hooks are guaranteed, and that a blocking
hook delays that command's `Done`, `Wait` and `Shutdown`.

**Verification**: Read against the code; the claims are checked by T1 and T2's tests.

**Notes.** Done. `doc.go` gained a "Learning that a command has ended" section contrasting the two
paths, and the `OnStart`/`OnExit` field comments now say outright that they are dropped when the
event loop falls behind. The previous wording — "keep the callback non-blocking" — asked for the
right behaviour without saying what happens if you ignore it.

The first draft claimed a panicking exit hook leaves the command unreaped. It does not: `cmd.Wait()`
has already returned by then. The real consequence is worse and now stated — the panic is on the
reaper goroutine, so it ends the process.

### T4: Report whether an exit was asked for

**Outcome**: `ExecCmd.StopRequested() bool` reports whether `Stop` or `KillAll` requested
termination while the command was observed running. Sticky, so it still answers after the command
has gone.

**Verification**: Tests for a command that ended on its own, one that was stopped, one killed
through `KillAll`, and — the case that matters — one that crashed and was only then stopped. A test
asserts the flag is readable from the exit hook.

**Notes.** Done. `ExecCmd` already had a transient `stopping` field, cleared once the stop
completes; this is the durable fact next to it.

The flag is set after `Stop`'s liveness check rather than on entry. Setting it on entry would mark a
command that had already crashed, and the whole point of the flag is to tell a crash from a
shutdown. Mutation-tested: moving the assignment above the check fails
`TestStopRequestedStaysFalseWhenTheCommandWasAlreadyGone` with `StopRequested() is true for a
command that had already exited`.

`KillAll` bypasses `Stop` and calls `forceKill` directly, so it marks the command itself. This is why
the flag belongs here rather than in a consumer's wrapper: a consumer cannot see `KillAll`, and
`Shutdown` reaches processes through `StopAll` without going through anything a consumer wrote.

Deliberately false for a command signalled from outside the `Procman` — a stray `kill(1)` is a crash
as far as this library is concerned, because it is not something the library asked for.

### T5: Report when the command started and exited

**Outcome**: `ExecCmd.ExitedAt()` and `ExecCmd.StartedAt()`, both zero before the event they name.

**Verification**: Tests that each is zero beforehand, that each falls between the call that should
have set it and the moment after, that `StartedAt` precedes `ExitedAt`, and that the exit hook can
read `ExitedAt`.

**Notes.** Done. `ExitedAt` is written in the reaper under `c.mu`, alongside the status it already
sets, so there is one write and it happens before the exit hook and before `Done` closes.
`startedAt` was already being recorded and simply had no accessor.

`ExitedAt` is recorded after `exec.Cmd.Wait` and platform cleanup complete, not when the kernel
ended the process — the doc comment says so, because the difference is invisible from here and a
caller measuring shutdown latency would otherwise be measuring the reaper's scheduling.

The alternative was leaving it to each consumer, which is what the first consumer was doing: a mutex
and a hook body on `zirafa-core`'s `ManagedProcess` existing only to timestamp something `procman`
was already standing over. Both are now gone.

## Risks and Compatibility

- **Additive only.** New methods and new options; no existing signature or behaviour changes.
- **A blocking hook is a footgun.** It delays that command's `Done()`, `Wait()` and therefore
  `Shutdown()`. Contained to one command by design, and documented, but real.
- **A panicking exit hook ends the process.** It runs on the reaper goroutine, so an unrecovered
  panic there is unrecovered everywhere. Not defended against: recovering in a supervision library
  would hide the bug that caused it, and the manager-wide callbacks behave the same way today. A
  panicking *start* hook is ordinary — it unwinds through `Start` into the caller, who can recover.
- **Two ways to learn the same thing.** A consumer may now wire both paths and be surprised that
  only one is reliable. T3 exists for this.

## Verification

1. `gofmt`, `go vet ./...` clean.
2. `go test ./... -count=1 -race` clean on darwin.
3. `GOOS=linux go build ./...` and `GOOS=windows go build ./...` clean.
4. The dropped-event case is tested directly: enough simultaneous exits to overflow the 256-entry
   channel, with every per-command hook still observed.
5. Guards mutation-tested.

**Result** (2026-08-05): focused lifecycle metadata tests passed 10 consecutive race-enabled runs;
the full race suite passed on darwin/arm64 and Linux; `go vet ./...` was clean; and Windows and
FreeBSD test binaries cross-compiled successfully.

## Rollout

Additive; no migration. The first consumer is `zirafa-core`, which deletes its duplicate reaper in
the same change.

## Decision Log

- **2026-08-05** — Per-command hooks are called directly from the command's own goroutine rather
  than routed through `evtCh`. Queuing is what makes the existing callbacks droppable; putting the
  new ones on the same queue would reproduce the defect being fixed.
- **2026-08-05** — The manager-wide callbacks are left alone rather than made undroppable. They are
  documented as asynchronous observability and something in the library should stay cheap and
  non-blocking. Making them reliable as well would leave no way to say "tell me if you can".
- **2026-08-05** — `Done()` returns the existing `doneChan` rather than a new channel, so there is
  one exit signal, closed in one place, and `Shutdown`'s existing read and a consumer's read cannot
  disagree.
- **2026-08-05** — A panicking hook is not recovered. A supervision library that swallows a panic
  turns a visible crash into a hung shutdown.
- **2026-08-05** — `Start` was split into a locked `start` and an unlocked hook call rather than
  calling the hook in place. Review found the in-place version deadlocked a hook that registered a
  command, because `Start` holds `Procman.mu.RLock` throughout. Reproduced: `NewExecCmdFromCmd`
  blocked on `Procman.mu.Lock` while `Start` held the read lock.
- **2026-08-05** — The "-1 for a signal" assertion is Unix-only. `os.ProcessState.ExitCode` reports
  -1 for a signalled child on Unix; Windows reports the status `TerminateProcess` was given. The
  portable assertion is that `ExitCode` agrees with `GetProcessState().ExitCode`, which is all the
  method promises.
- **2026-08-05** — `stopRequested` lives on `ExecCmd` rather than in the consumer. A wrapper can
  only see the stops it issues itself; `KillAll` calls `forceKill` directly and `Shutdown` goes
  through `StopAll`, so a command can be signalled by this library without the consumer's wrapper
  ever being told. Only `ExecCmd` sees every path.
- **2026-08-05** — Capturing child output stays with the consumer. Prefixes, tail limits and log
  sinks are application policy, and `exec.Cmd.Stdout` is already a caller-supplied `io.Writer`, so
  there is nothing here the library alone can know. The hazard the library does create — the reaper
  calling `Wait` and closing `StdoutPipe` under a reader — is documented on `NewExecCmdFromCmd`
  instead.
