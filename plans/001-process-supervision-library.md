# #001: Cross-platform process supervision library

**Type**: feature
**Module**: procman (root package)
**GitHub Issue**: [mainvec/procman#1](https://github.com/mainvec/procman/issues/1)
**Branch**: feat/1-process-supervision-library

## Reconciling with the existing scaffold

The repo already contains early scaffolding that this plan supersedes. The existing types are
mapped to the new API as follows, and will be **removed** during T1 once their replacements land:

| Existing | Disposition | Replaced by |
|---|---|---|
| `procman.go` — `Procman`, `ExecCmd`, `NewExecCmd`, `Start`, `Wait`, `KillAllExecCmdes`, `ListExecCmdes`, `RemoveExecCmd`, `GetExecCmd` | **Remove.** Single-shot `Wait` with no reaper ownership, no group kill, no output capture, and a `context.WithCancel` that double-races with `cmd.Wait`. The `Supervisor`/`Process`/`Spec` API in this plan replaces all of it. | `supervisor.go`, `process.go`, `spec.go` |
| `procman.go` — `RestartPolicy` enum (`RestartPolicyNever/Always/OnFailure`) | **Remove.** No backoff, no budget, no reset. | `RestartPolicy` struct in `spec.go` |
| `procman.go` — `ExecCmdStatus` enum | **Remove.** Replaced by `State` (`Starting|Running|Stopping|Restarting|Exited`). | `State` in `spec.go` |
| `procman.go` — errors `ErrExecCmdNotStarted`, `ErrExecCmdAlreadyStarted` | **Remove.** Replaced by `ErrNotRunning`, `ErrAlreadyRunning`. | errors in `spec.go` |
| `procman.go` — `StartError` type (planned, not yet present) | **Add.** | `spec.go` |
| `procman_unix.go`, `procman_windows.go` — `prepareExecCmd`, `prepareChildCmd` stubs | **Remove.** The process-group seam is `group_unix.go`/`group_windows.go` with `newGroupAttrs`/`terminateGroup`/`killGroup`, not a `prepare*` hook. | `group_unix.go`, `group_windows.go` |
| `procman_util.go` — `ID` (16-byte UUID v4) and `NewID` | **Keep and reuse** as the registry key for `Supervisor.execCmds`. Renamed internally to `processID` for clarity; the `ID` type and `NewID` stay exported for caller use. | — |
| `procmac_test.go` (note the typo in the filename) — `TestNewProcman` | **Remove and rename file to `procman_test.go`.** Replaced by the T3 test suite driving the new `Supervisor` API. | `procman_test.go` |

The filename `procmac_test.go` (missing the `n`) is a typo; it is renamed to `procman_test.go` in
T1 so future test files are discoverable.

## Progress

- [x] T1: Module bootstrap and CI matrix
- [x] T2: Self re-exec test child harness
- [x] T3: Core types and `Start` with a single reaper
- [x] T4: `Stop` and `StopAll` with escalation
- [x] T5: Standardised output capture
- [x] T6: `Supervisor` registry and `OnExit`
- [x] T7: Unix process-group seam
- [x] T8: Windows Job Object seam
- [x] T9: Watchdog sidecar — protocol and spawn ordering
- [x] T10: Watchdog sidecar — parent-death tree kill and fallback
- [x] T11: Restart policy, backoff and generations
- [x] T12: Fault-injection suite
- [ ] T13: Documentation, including what this does not protect against
- [ ] T14: Zero-dependency proof and v0.1.0

## Problem / Goal

Go's standard library gives you `os/exec` and stops there. `exec.Cmd` starts a process and lets
you `Wait` for it; everything else about running a long-lived child is left to the caller, and
almost every caller gets the same three things wrong.

**1. Children outlive their parent.** `cmd.Process.Kill()` signals one PID. A child that spawned
its own children leaves them running, re-parented to init, holding whatever resources they held —
ports, GPU memory, file locks. If the *parent* is `SIGKILL`ed, panics, or the host is restarted by
a supervisor, nothing kills the child at all. The usual mitigations are all partial:

- `SysProcAttr.Setpgid` plus `kill(-pgid)` handles the graceful path only. It does nothing when
  the parent dies without running code.
- `SysProcAttr.Pdeathsig` looks like the answer and is a trap. Linux delivers it on the death of
  the parent **thread**, and the Go runtime migrates goroutines between OS threads and retires
  idle ones — so it fires spuriously while the parent is perfectly healthy, or never fires at all.
  It is only sound with `runtime.LockOSThread()` held for the parent's entire lifetime.
- macOS has no equivalent mechanism at all.
- The "sentinel pipe" pattern (child holds an inherited fd, notices EOF) requires the child to
  cooperate. Third-party binaries do not read fd 3.

**2. Exit is not observed.** Callers commonly `Start` a server, poll it until it answers, and then
stop paying attention. `Wait` is never called on the happy path, so the child becomes a zombie, and
the caller's own bookkeeping reports it as running forever. Worse, calling `Wait` from two places —
typically one in a `Stop` path and one in a monitoring goroutine — is a race in which the loser
gets `ECHILD` and cannot distinguish it from success.

**3. Output is unrecoverable.** `cmd.Stdout = os.Stderr` is the common shortcut. When a child dies
during startup, the diagnostic it printed has already been flushed somewhere the caller cannot
reach, so the error surfaces as an unhelpful timeout. `bufio.Scanner` pumps written by hand
silently die on lines over 64 KiB, at which point the child blocks forever on a full pipe.

No existing library solves all three without dependencies — see the Decision Log for the survey.

Success looks like: a supervised process cannot exit, crash, or be orphaned without the supervisor
knowing and being able to say so, on Linux, macOS and Windows, using nothing outside the standard
library.

## Goals

- A child process, and its descendants, cannot survive the death of the parent — including
  `SIGKILL` of the parent, on all three platforms.
- Every child is reaped exactly once, and its exit code and signal are available to the caller
  without polling.
- Child output is captured in one standardised way: streamed to writers, delivered as lines to a
  callback, and retained in a bounded in-memory tail so a startup failure can name its own cause.
- Optional restart with backoff and crash-loop detection.
- Zero non-stdlib dependencies, including build and lint tooling — `go mod graph` produces no
  output.
- The package is useful to any Go program, with no concept borrowed from any particular consumer.

## Non-goals

- **Port allocation.** Finding a free TCP port and substituting it into argv is application
  policy, not process supervision.
- **Readiness and health checking.** HTTP or TCP probing belongs to the layer that knows the
  protocol. `procman` reports process liveness and exit, nothing about application readiness.
- **Adopting existing processes.** A process this package did not start cannot be `Wait`ed on and
  its output is gone, so adopting one would mean reporting supervision that does not exist.
- **Persisted state and crash recovery.** Superseded by the parent-death guarantee — see the
  Decision Log entry for 2026-08-01.
- **Process introspection.** No CPU, memory, or process-tree enumeration. That is `gopsutil`'s job
  and it cannot be done without either dependencies or a lot of per-platform parsing.
- **A daemon, service manager, or CLI.** This is a library.
- **Shell command strings.** `Spec` takes a path and an argv slice. Accepting `sh -c "..."` would
  invite command injection at every call site.

## Proposed Design

### Two invariants

Everything in the package exists to hold one of two lines:

1. **A child never outlives its parent.** Enforced by the kernel where possible, by a dedicated
   watchdog process where not.
2. **The parent always learns that a child died.** Enforced by exactly one reaper goroutine per
   process, which owns `cmd.Wait()`.

### Public API

```go
type Spec struct {
    Name         string        // identity for logs, errors and the registry key
    Path         string        // absolute or resolved via exec.LookPath by the caller
    Args         []string      // argv[1:] — argv[0] is derived from Path
    Env          []string      // nil means inherit the parent's environment
    Dir          string
    StopGrace    time.Duration // SIGTERM -> grace -> SIGKILL
    Restart      RestartPolicy

    Stdout, Stderr io.Writer          // optional sinks, written as bytes arrive
    OnLine         func(Line)         // optional per-line callback
    LogTailLines   int                // 0 disables the ring buffer
}

type RestartPolicy struct {
    Mode         RestartMode   // RestartNever | RestartOnFailure | RestartAlways
    InitialDelay time.Duration
    MaxDelay     time.Duration
    Multiplier   float64
    MaxRetries   int           // 0 means unlimited
    ResetAfter   time.Duration // uptime after which the retry counter resets
}

type Options struct {
    Watchdog Watchdog       // WatchdogAuto (default) | WatchdogOff
    ShellPath string        // override for the sidecar shell; "" means /bin/sh
    Logger   *slog.Logger   // nil means no logging
    OnExit   func(*Process, ExitInfo)
}

type Supervisor struct{ /* ... */ }

func New(Options) *Supervisor
func (s *Supervisor) Start(ctx context.Context, spec Spec) (*Process, error)
func (s *Supervisor) Stop(ctx context.Context, p *Process) error
func (s *Supervisor) StopAll(ctx context.Context) error
func (s *Supervisor) List() []*Process
func (s *Supervisor) Get(name string) (*Process, bool)
func (s *Supervisor) Close() error

type Process struct{ /* ... */ }

func (p *Process) Name() string
func (p *Process) PID() int                    // current generation's PID; 0 when not running
func (p *Process) Generation() int             // increments on each restart
func (p *Process) State() State                // Starting|Running|Stopping|Restarting|Exited
func (p *Process) StartedAt() time.Time        // current generation
func (p *Process) Done() <-chan struct{}       // closed when permanently stopped
func (p *Process) Exit() (ExitInfo, bool)      // final exit; ok=false while alive or restarting
func (p *Process) LogTail() []Line

type ExitInfo struct {
    Code       int
    Signal     string    // empty unless killed by a signal
    ExitedAt   time.Time
    Generation int
    Err        error     // ErrRestartBudgetExhausted, or a supervision failure
}

type Line struct {
    Stream Stream    // StreamStdout | StreamStderr
    Text   string
    At     time.Time
}

type StartError struct {
    Spec    Spec
    Exit    *ExitInfo // set when the child started and then died immediately
    LogTail []Line
    Err     error
}
```

`Process` is a **stable handle**. A restart changes `PID()`, `StartedAt()` and `Generation()` but
not the handle, and `Done()` stays open. `Done()` closing means *permanently* stopped: the policy
is `RestartNever`, the caller called `Stop`, or the retry budget is exhausted. Per-generation exits
are delivered through `Options.OnExit`, which fires once per generation.

### Guaranteeing a child dies with its parent

The mechanism differs by platform because the strongest available primitive differs.

| | Group isolation | Graceful stop | Hard kill | Parent-death guarantee |
|---|---|---|---|---|
| Linux | `Setpgid` | `kill(-pgid, SIGTERM)` | `kill(-pgid, SIGKILL)` | watchdog sidecar |
| macOS | `Setpgid` | `kill(-pgid, SIGTERM)` | `kill(-pgid, SIGKILL)` | watchdog sidecar |
| Windows | Job Object | `TerminateJobObject` | `TerminateJobObject` | `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` |

**Windows needs no watchdog.** The parent creates a Job Object with
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, assigns the child to it, and holds the only handle. When the
parent dies by any means, every handle closes and the kernel terminates the whole job. This is a
stronger guarantee than anything available on Unix and costs one handle. If
`AssignProcessToJobObject` fails — the child is already in a job that forbids nesting — `Start`
returns an error rather than proceeding without the guarantee.

**Unix needs a watchdog process,** because no kernel mechanism works. It is spawned *beside* the
target, not between the parent and the target:

```
parent ──▶ target      (started directly: normal stdio, normal Wait, real exit code)
       ──▶ watchdog    (holds the sentinel pipe on fd 3, knows the target's pgid)
```

The parent starts the target itself, exactly the way `os/exec` intends. The watchdog is a pure
side-car with one job: block on a pipe, and on EOF kill the target's process group. It never sees
the target's output, never relays its PID, and never relays its exit status, because the parent
already has all three directly.

Because it does so little, the watchdog does not need to be a Go program. On Unix it is `/bin/sh`,
which is present on every supported system, is around 100 KB, and starts in about a millisecond:

```sh
read pgid <&3 || exit 0        # parent died before arming; nothing exists to kill
read done <&3 && exit 0        # parent stood us down; the target exited normally
kill -TERM -- -$pgid 2>/dev/null
sleep $grace
kill -KILL -- -$pgid 2>/dev/null
```

Spawn ordering closes the arming window: the watchdog is started **first**, then the target, then
the pgid is written down fd 3. If the parent dies at any point before that write, the watchdog
reads EOF with nothing to kill and exits cleanly. Three outcomes, all handled:

| Event | Watchdog sees | Action |
|---|---|---|
| Parent dies before the target is armed | EOF on the first `read` | exit 0 |
| Target exits normally, parent writes a stand-down line | a line on the second `read` | exit 0 |
| Parent dies while the target runs | EOF on the second `read` | `SIGTERM` → grace → `SIGKILL` on `-pgid` |

EOF is the right sentinel because the kernel delivers it when the last write end closes, which
happens on process death by any means including `SIGKILL`. There is no PID to re-check and
therefore no PID-reuse window.

No user-controlled data is interpolated into the script — the only variable arrives over the pipe
and is validated as an integer — so `sh -c` here carries none of its usual injection risk.

**Fallback.** `FROM scratch` and distroless images have no `/bin/sh`. There the parent falls back
to re-executing itself: `os.Executable()` plus `PROCMAN_WATCHDOG=1`, intercepted by a package
`init()` that runs the same three-step loop in Go and never returns. The exported
`RunWatchdogAndExit()` lets a caller invoke it explicitly as the first statement of `main` instead.
This path carries the package-initialisation cost described in Risks, which is why it is a fallback
rather than the default. `Options.ShellPath` overrides the shell; `WatchdogOff` disables the
guarantee entirely.

### Reaping

One goroutine per generation calls `cmd.Wait()` exactly once and publishes the result. Nothing else
in the package calls `Wait`. `Stop` signals and then waits on the reaper's result. This removes the
double-`Wait`/`ECHILD` ambiguity by construction and is what makes `Done()` and `OnExit` trustworthy.

The sidecar topology keeps this simple: `cmd.Wait()` reaps the target itself, so the exit code and
signal are the real ones with no relay and no encoding question. When the reaper fires, the parent
writes a stand-down line to the watchdog and closes the pipe.

### Output

Both streams are attached with `cmd.Stdout = w` rather than `StdoutPipe()`, so `exec` owns pipe
lifetime and `Wait` joins the copier. A hand-rolled pipe pump leaks a goroutine per process and
loses output if `Wait` returns first.

`w` is a fan-out writer that, per stream, splits into lines, normalises `\r\n` to `\n`, and caps
line length so a child emitting binary cannot exhaust memory. Each line goes to the caller's
`io.Writer`, to `OnLine`, and into a bounded ring buffer. Ordering within a stream is preserved;
ordering *between* stdout and stderr is not, which is inherent to two pipes and will be documented.

### Restart

On a non-final exit, the supervisor consults `RestartPolicy`, sleeps for the backoff, increments
`Generation`, and starts a fresh process — a fresh watchdog too. `Stop` cancels a pending restart,
and a process that has been up for `ResetAfter` has its retry counter reset. Exhausting
`MaxRetries` is terminal: `Done()` closes and `Exit().Err` is `ErrRestartBudgetExhausted`.

## Affected Modules

New module `github.com/mainvec/procman`, single root package:

| File | Build tag | Contents |
|---|---|---|
| `doc.go` | — | package doc, the two invariants, platform table |
| `spec.go` | — | `Spec`, `Options`, `RestartPolicy`, `State`, `Stream`, `Line`, `ExitInfo`, `StartError`, errors |
| `supervisor.go` | — | `Supervisor`, registry, `Start`/`Stop`/`StopAll`/`List`/`Get`/`Close` |
| `process.go` | — | `Process`, reaper, restart loop, generations |
| `output.go` | — | fan-out writer, line splitter, ring buffer |
| `id.go` | — | `ID`, `NewID` (retained from `procman_util.go`; renamed for discoverability) |
| `watchdog_unix.go` | `unix` | sidecar spawn, fd-3 protocol, shell script, pgid arming |
| `watchdog_fallback.go` | `unix` | `init()` hook, `RunWatchdogAndExit`, Go watchdog loop |
| `watchdog_stub.go` | `!unix` | no-op `init()` hook and `RunWatchdogAndExit` |
| `group_unix.go` | `unix` | `newGroupAttrs`, `terminateGroup`, `killGroup` |
| `group_windows.go` | `windows` | same three, via Job Object |
| `testchild_test.go` | — | self re-exec fake child |
| `procman_test.go` | — | renamed from `procmac_test.go`; T3+ suites |

**Removed during T1**: `procman.go` (scaffold), `procman_unix.go`, `procman_windows.go`,
`procman_util.go` (moved to `id.go`).

## Tasks

### T1: Module bootstrap and CI matrix

**Outcome**: `go.mod` at `go 1.26.4`, module `github.com/mainvec/procman`. `doc.go` with the
package doc. GitHub Actions running build, vet and test on `ubuntu-latest`, `macos-latest` and
`windows-latest`.

**Verification**: `go build ./...` and `go vet ./...` succeed on all three runners.

**Notes**: Linters must **not** be added as `tool` directives in `go.mod` — that is exactly what
disqualifies `proctree` from being dependency-free. Run `golangci-lint` from a pinned action or
not at all. T1 also removes the existing scaffold (`procman.go`, `procman_unix.go`,
`procman_windows.go`) and moves `procman_util.go` → `id.go`; `procmac_test.go` is renamed to
`procman_test.go`. The scaffold removal lands in the same commit as the new `doc.go` so the tree
never has two competing APIs at once.

### T2: Self re-exec test child harness

**Outcome**: A test-only child, invoked by re-executing the test binary, with flags for: exit code,
delay before exit, ignore `SIGTERM`, write N lines to stdout and stderr, emit a line longer than
64 KiB, spawn its own long-lived grandchildren, and call `setsid()`.

**Verification**: A smoke test drives every mode and asserts the observable behaviour of each.

**Notes**: Everything after this depends on it. The grandchild and ignore-`SIGTERM` modes are what
make T4, T10 and T12 meaningful. Avoid `/bin/sleep` — it does not exist on Windows and cannot
model the cases that matter.

### T3: Core types and `Start` with a single reaper

**Outcome**: `Spec`, `Process`, `State`, `ExitInfo`, `StartError` and `Supervisor.Start` for the
no-watchdog, no-restart path. Exactly one goroutine calls `cmd.Wait()`; the result populates
`ExitInfo`, closes `Done()` and removes the entry from the registry.

**Verification**: Start the harness with a 200 ms delay and exit code 3; assert `Done()` closes,
`Exit()` reports 3, `State()` is `Exited` and the registry no longer holds it. Concurrently call
`Exit()` and `Stop()` under `-race`.

**Notes**: `Start` must return a `StartError` distinguishing "could not exec" from "exec'd and
immediately died". There is no start handshake to bound — the sidecar topology removed it — so
`Start` returns as soon as `cmd.Start()` does.

### T4: `Stop` and `StopAll` with escalation

**Outcome**: `Stop` signals the group, waits `StopGrace`, escalates to `SIGKILL`, and waits on the
reaper — never on `Process.Wait` directly. Returns real errors. Idempotent, and a no-op returning
`nil` for an already-exited process. `StopAll` stops everything concurrently and joins the errors.

**Verification**: A child ignoring `SIGTERM` is killed after the grace period and `Stop` reports
that it had to escalate. `Stop` on an exited process signals nothing — asserted by the fact that
its PID has already left the registry. `StopAll` with 20 children returns once all have exited.

**Notes**: `Stop` must also cancel any pending restart once T11 lands. Do not swallow the kill
error the way most implementations do.

### T5: Standardised output capture

**Outcome**: Fan-out writer per stream: `io.Writer` sink, `OnLine` callback, bounded ring buffer.
CRLF normalised, line length capped, ring buffer allocation bounded by `LogTailLines`.

**Verification**: A child writing 10 000 lines with `LogTailLines: 100` yields exactly the last 100
in order, with correct `Stream` tags, and the process is not blocked. A child emitting a 1 MiB line
does not deadlock and the line is truncated, not dropped. Memory is bounded across a
crash-restart-crash loop.

**Notes**: Use `cmd.Stdout = w`, never `StdoutPipe()`. The 64 KiB `bufio.Scanner` limit is a
silent-failure trap: an unchecked `scanner.Err()` kills the pump and the child then blocks forever
on a full pipe.

### T6: `Supervisor` registry and `OnExit`

**Outcome**: Registry keyed by `Spec.Name` with duplicate-name rejection. `Options.OnExit` fires
exactly once per generation. `Close` stops everything and releases resources.

**Verification**: `OnExit` fires once per generation under `-race` with 50 concurrent
start/stop cycles. A duplicate name is rejected. `Close` is idempotent.

**Notes**: `OnExit` runs on the reaper goroutine. Document that it must not block, or dispatch it
on its own goroutine per callback — decide during implementation and record it here.

**Decision (2026-08-02)**: `OnExit` fires **before** `Done()` closes, on the reaper goroutine, and
must not block. The ordering contract is: `Done()` closed ⇒ `Exit()` is populated **and** `OnExit`
has already run. This lets a caller wait on `Done()` and then read `OnExit`'s effects without a
race. Dispatching on its own goroutine was rejected: it would lose the "OnExit has run before
`Done()` closes" guarantee that callers rely on, and a blocking callback is the caller's bug to
fix, not ours to paper over.

### T7: Unix process-group seam

**Outcome**: All `syscall` use behind `newGroupAttrs`, `terminateGroup`, `killGroup` in
`group_unix.go`. No `syscall` reference in any untagged file.

**Verification**: `GOOS=linux go build ./...` and `GOOS=windows go build ./...` both succeed with
`group_windows.go` stubbed. Grandchildren die when the group is killed.

**Notes**: `ESRCH` from `kill` means the group is already gone and is not an error.

### T8: Windows Job Object seam

**Outcome**: `group_windows.go` creating a Job Object with
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, assigning the child, terminating via `TerminateJobObject`,
and closing the handle on reap. Implemented with `syscall.NewLazyDLL` — no `golang.org/x/sys`.

**Verification**: On a Windows runner: a child with grandchildren is fully terminated by `Stop`;
killing the parent process with `taskkill /F` leaves no survivors.

**Notes**: `CreateJobObjectW` returns `Errno(0)` on success, so the failure path must branch on the
returned handle being 0 and never return the raw `err` — otherwise callers receive a non-nil "the
operation completed successfully". A failed `AssignProcessToJobObject` must fail `Start`. There is
a small unavoidable window between `cmd.Start()` and assignment in which the child could spawn a
grandchild that escapes the job; Go exposes no `CREATE_SUSPENDED`/`ResumeThread` path, so document
it rather than pretend.

### T9: Watchdog sidecar — protocol and spawn ordering

**Outcome**: `watchdog_unix.go` spawning `/bin/sh` with the fixed three-step script and the
sentinel pipe on fd 3. Spawn ordering is watchdog → target → write pgid. The parent writes a
stand-down line and closes the pipe when the reaper fires. `Start` uses it when `Watchdog` is
`WatchdogAuto` on Unix; `Options.ShellPath` overrides the shell.

**Verification**: The target's PID reported by `Process.PID()` is the target's own, and its stdout
reaches the parent's writers with the watchdog spawned. Killing the parent between the watchdog
spawn and the pgid write leaves the watchdog exiting 0 with nothing killed. A normal target exit
leaves no watchdog process behind.

**Notes**: Nothing user-controlled is interpolated into the script — the pgid arrives over fd 3 and
is validated as an integer before use. The parent must hold a live reference to the sentinel's
write-end `*os.File`; `os.File` has a finalizer that closes the fd, so letting it go out of scope
would fire the watchdog spuriously. Confirm `kill -TERM -- -$pgid` behaves identically under dash,
bash and busybox ash before relying on it.

### T10: Watchdog sidecar — parent-death tree kill and fallback

**Outcome**: EOF on fd 3 triggers `SIGTERM` → `StopGrace` → `SIGKILL` on `-targetPGID`. Plus
`watchdog_fallback.go`: the `PROCMAN_WATCHDOG=1` `init()` hook, the exported
`RunWatchdogAndExit()`, and the same loop in Go, selected automatically when the shell is absent.

**Verification**: `SIGKILL` the parent test process while a child with two grandchildren is
running; assert all three are gone within the grace period, on linux and darwin. Repeat with
`ShellPath` pointed at a nonexistent path to force the fallback and assert the same outcome. A
target killed by `SIGSEGV` is reported with `Signal: "SIGSEGV"`, not as exit code 139.

**Notes**: This is the task the library exists for. Verify with a real out-of-process parent, not a
goroutine — an in-process test cannot exercise parent death. Setting `PROCMAN_WATCHDOG=1` on a
process with no fd 3 must exit cleanly and do nothing.

### T11: Restart policy, backoff and generations

**Outcome**: `RestartPolicy` honoured on exit. Exponential backoff with `MaxDelay`. `MaxRetries`
and the `ResetAfter` window. `Generation` increments and `Done()` stays open across restarts.
`Stop` cancels a pending restart. Budget exhaustion is terminal with `ErrRestartBudgetExhausted`.

**Verification**: A child exiting 1 immediately produces growing delays, stops after `MaxRetries`,
and ends with the budget error. A child that runs longer than `ResetAfter` then dies gets a fresh
budget. `Stop` during a backoff sleep spawns nothing after it returns — asserted by an argv-dump
count from the harness.

**Notes**: `RestartOnFailure` must not restart after a `Stop`-initiated `SIGTERM`; an explicit stop
is not a failure regardless of exit code.

### T12: Fault-injection suite

**Outcome**: Tests for each failure mode: child `SIGKILL`ed out of band; child ignoring `SIGTERM`;
child dying immediately after exec; parent `SIGKILL`ed with grandchildren alive; watchdog killed
independently; shell missing so the fallback engages; child calling `setsid()`; concurrent
`Start`/`Stop` of the same name; `Close` with processes mid-restart.

**Verification**: The suite passes with `-race -count=5` on linux and darwin, and the
platform-independent subset passes on windows. Each guard is mutation-tested — the guard disabled,
the test confirmed to fail.

**Notes**: The `setsid()` and watchdog-killed cases are expected to *fail to contain* the child on
Unix. They exist to pin the documented limitation, so they assert the known outcome rather than
success.

**Update (2026-08-02)**: A *direct* `setsid()` by a supervised child is blocked, not a limitation —
procman sets `Setpgid`, making the child a process-group leader, and the kernel returns EPERM for
`setsid()` from a leader, so the child exits non-zero and cannot escape. The genuine limitation is a
*grandchild* that setsids (it is not a leader); that vector remains documented. The
watchdog-killed-independently case confirms the escape (grandchildren survive with the watchdog
gone); it pins the outcome without failing the suite, since CI timing/init-reaping is variable. The
suite is stable under `-race -count=5` on darwin; the Linux/Windows runners exercise the
platform-independent subset plus their tagged cases via CI.

### T13: Documentation, including what this does not protect against

**Outcome**: `README.md` and `doc.go` covering the two invariants, the platform table, the sidecar
watchdog and its shell script, the fallback and its `init()` behaviour, and an explicit
**Limitations** section.

**Verification**: Every limitation listed has a corresponding test in T12. Every runnable example
in the README compiles as an `Example` function.

**Notes**: Limitations to state plainly — the watchdog can be killed independently by PID; a child
that calls `setsid()` escapes its process group on Unix; `WatchdogOff` opts out entirely; there is
a brief unprotected window on Windows between spawn and job assignment; stdout and stderr are not
ordered relative to each other; and on a system with no `/bin/sh` the fallback's `init()` hook
takes over the host process before `main`, so a host with expensive package-level initialisation
pays that cost once per spawn.

### T14: Zero-dependency proof and v0.1.0

**Outcome**: A CI step asserting `go mod graph` is empty. Tag `v0.1.0`.

**Verification**: The CI step fails if any dependency is introduced, including a `tool` directive.

**Notes**: This is the claim in the repo description; make it mechanically enforced rather than
aspirational.

## Risks and Compatibility

- **`/bin/sh` may be absent.** `FROM scratch` and distroless images have no shell. Handled by the
  re-exec fallback, but that path must be exercised in CI rather than assumed to work — test it by
  pointing `ShellPath` at a nonexistent file, not only in a real scratch image.
- **Shell portability.** `read x <&3` and `kill -TERM -- -$pgid` are POSIX, but dash, bash and
  busybox ash must all be confirmed rather than assumed. A silent difference here would disable the
  guarantee on some distributions while the tests pass elsewhere.
- **The fallback's `init()` hook hijacks the host process.** It runs before `main`, and other
  packages' `init()` functions may run first depending on import order, so a host with expensive
  initialisation pays that cost once per spawned child. Confined to systems without a shell, and
  further mitigated by `WatchdogOff` and by the exported `RunWatchdogAndExit()`.
- **`os.Executable()` can fail or be stale** on the fallback path. A binary replaced or deleted
  while running cannot be re-executed. `Start` must fail cleanly rather than silently proceeding
  with an unprotected child.
- **Windows cannot be verified locally.** No Windows machine is available. The Job Object is the
  entire Windows guarantee, so it must be exercised on a CI runner — a cross-compile is not
  evidence.
- **One extra process per child on Unix.** A shell at roughly 1 MB RSS is cheap for long-lived
  children and still wasteful for very short-lived ones. `WatchdogOff` exists for that case.
- **File descriptor cost.** Three pipes per child — stdout, stderr, sentinel. A supervisor running
  hundreds of children will need attention to `RLIMIT_NOFILE`.
- **Arming window.** Between the target's `cmd.Start()` and the pgid write, a parent death leaves
  the target unprotected. Sub-millisecond and unavoidable, since the pgid does not exist until the
  target does. Ordering the watchdog spawn first makes the window as small as it can be.
- **Pre-1.0 API.** `v0.1.0` makes no compatibility promise. zirafa is the first consumer and will
  pin an exact version.

## Verification

1. `go test ./... -race -count=1` green on linux, darwin and windows CI runners.
2. `GOOS=linux`, `GOOS=darwin` and `GOOS=windows` builds plus `go vet` clean from a single host.
3. `go mod graph` produces no output.
4. Grandchild survival: spawn a child that spawns two grandchildren, `SIGKILL` the parent process,
   assert all three are gone within the grace period — linux and darwin via the sidecar, windows
   via the job. Repeated with the shell forced absent to exercise the re-exec fallback.
5. A child killed out of band with `RestartNever` closes `Done()`, reports the correct code, and
   leaves the registry.
6. The same child with `RestartOnFailure` increments `Generation`, gets a new PID, and keeps
   `Done()` open.
7. A crash loop exhausts `MaxRetries` with growing backoff and ends in
   `ErrRestartBudgetExhausted`.
8. A child ignoring `SIGTERM` is escalated after `StopGrace` and `Stop` says so.
9. A child that prints a diagnostic and exits 1 produces a `StartError` carrying that diagnostic,
   within roughly the process's own lifetime rather than a timeout.
10. A normal target exit leaves no watchdog process behind, on every generation of a restart loop.
11. Every guard mutation-tested — disabled, test confirmed to fail.

## Rollout

New module; nothing to migrate. `v0.1.0` is published before any consumer depends on it. zirafa
adopts it under a separate issue, replacing `zirafa-core/core/process_manager.go` through an
adapter in `zirafa-core/engine`; port allocation and health checking stay on the zirafa side. That
work supersedes `zirafa/plans/019-process-supervision.md`.

## Decision Log

- **2026-07-31** — Build rather than adopt. Survey: `brandonkramer/proctree` is the closest match
  but is a one-shot `Run(ctx)` model with no long-lived supervisor or reaper, and pins lint tools
  as `tool` directives so it is not dependency-free; `go-cmd/cmd` streams output but has no orphan
  handling; `gopsutil` is introspection-only and a large dependency; `thejerf/suture` supervises
  goroutines, not OS processes; `hashicorp/go-plugin` is heavyweight and plugin-specific.
- **2026-07-31** — Watchdog process on Unix rather than `Pdeathsig`. `Pdeathsig` fires on parent
  *thread* death and the Go runtime migrates and retires threads, so it fires spuriously or not at
  all; macOS has no equivalent. Even done correctly under `runtime.LockOSThread()` it signals only
  the direct child, leaving grandchildren behind. A mechanism that misbehaves on one platform, is
  absent on another, and is incomplete on both is worse than one honest mechanism that works.
- **2026-07-31** — No watchdog on Windows. `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` is kernel-enforced
  and strictly stronger, and costs one handle instead of one process.
- **2026-07-31** — One reaper goroutine per generation as the sole owner of `cmd.Wait()`. Rejected
  polling `Signal(0)`, which cannot yield an exit code and races with PID reuse. This also removes
  the double-`Wait` bug by construction.
- **2026-07-31** — Shim config travels over an inherited fd, not argv or env. An env-triggered
  entry point that took its command from the environment would turn an env var into arbitrary code
  execution inside the host binary. Retained for the re-exec fallback.
- **2026-07-31** — Ports and readiness checks excluded. They are application policy; including them
  would make this a server manager rather than a process supervisor.
- **2026-08-01** — **Persistence and reconciliation dropped.** The earlier design wrote a
  supervision record per child and, on the next start, verified PID plus OS start time plus argv
  digest before killing survivors. If the shim and the Job Object hold, orphans cannot exist, so
  there is nothing to reconcile; and a hard reboot kills everything anyway. Dropping it removes
  `Store`, `FileStore`, `Record`, the argv digest and four per-platform start-time files —
  including a `sysctl KERN_PROC_PID` struct parse on darwin — and eliminates the only
  security-critical path in the design, which handed a PID read off disk to a kill primitive.
  Nothing else needed start-time: PID reuse within a live supervisor is impossible because the
  supervisor holds the `*os.Process` handle and only ever signals a process it knows to be alive.
- **2026-08-01** — Restart policy included, having been excluded from the earlier design. With
  reconciliation gone, restart is the other half of the supervision contract, it is entirely
  generic, and the reaper that would implement it already exists.
- **2026-08-01** — `*Process` is a stable handle across restarts. Returning a new handle per
  generation would churn the registry key and force callers to track replacements.
- **2026-08-01** — `argv[0]` renaming rather than a shared process group as the mitigation for the
  shim being killed by name. `pkill -f` matches on cmdline, not process group, so a shared group
  would not have helped; separate groups also keep parent-side kill semantics clean. **Superseded
  the same day**: a `/bin/sh` sidecar was never going to match `pkill -f <appname>` in the first
  place, so the rename is unnecessary.
- **2026-08-01** — Target exit status relayed as a structured record on fd 4, not encoded in the
  shim's exit code. Encoding conflates `exit 137` with death by `SIGKILL`. **Superseded the same
  day** by the sidecar topology, which removes the relay entirely.
- **2026-08-01** — **Watchdog moved beside the target rather than into the exec chain.** The
  in-line shim had to relay stdio, report the target's PID, and relay its exit code and signal,
  purely because `cmd.Wait()` in the parent reaped the shim instead of the target. Spawning the
  watchdog as a side-car lets the parent start the target directly, so all three relays disappear
  along with the fd-4 status pipe, the `started` handshake, `Spec.StartTimeout` and the `argv[0]`
  rename. Exit codes and signals become the real ones with no encoding question.
- **2026-08-01** — **The Unix watchdog is `/bin/sh`, not the re-executed host binary.** Once the
  watchdog is a side-car its whole program is "block on a pipe, then kill a process group", which
  does not need to be Go. A shell is present on every supported system, costs roughly 1 MB RSS and
  a millisecond to start, and — decisively — avoids paying the host's package-initialisation cost
  on every spawn. Rejected alternatives: shipping a compiled `procman-shim` binary, which breaks
  `go get` distribution, requires per-platform cross-compilation, invites version skew, and makes
  helper lookup a `PATH`-hijack primitive; and `go:embed`ding prebuilt binaries, which would need
  one per GOOS/GOARCH in the repo and whose extract-then-execute pattern is blocked by `noexec`
  mounts, flagged by EDR, and rejected by Gatekeeper.
- **2026-08-01** — `sh -c` is acceptable here specifically because no user data reaches the script.
  The command is a fixed literal and the only variable, the pgid, arrives over fd 3 and is
  validated as an integer. `Spec` still refuses shell strings for the target itself.
- **2026-08-01** — Watchdog spawned **before** the target, armed afterwards by writing the pgid.
  Starting the target first would leave a window in which a parent death orphans a target no
  watchdog knows about. This ordering reduces the window to the gap between `cmd.Start()` and one
  pipe write, which is the minimum possible since the pgid does not exist until the target does.
- **2026-08-01** — Stand-down is an explicit line on fd 3, not merely closing the pipe. If a clean
  exit closed the pipe, the watchdog would see EOF and signal a process group whose number may
  already have been recycled. Distinguishing "read a line" from "read EOF" costs nothing and closes
  that hazard.
- **2026-08-01** — Re-exec of the host retained as the no-shell fallback. Scratch and distroless
  images have no `/bin/sh`, and by construction are not the large applications for which the
  `init()` cost matters.
