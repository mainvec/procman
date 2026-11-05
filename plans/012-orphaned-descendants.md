# #012: Track and clean up descendants after a process-tree command exits on its own

**Type**: feature
**GitHub Issue**: [#12](https://github.com/mainvec/procman/issues/12)
**Branch**: feat/12-track-orphaned-descendants

## Progress

- [x] T1: Track a populated tree after its direct child is reaped; expose `TreeDone`
- [x] T2: Let `Stop`, `StopAll`, `KillAll` and `Shutdown` reach tracked descendants
- [x] T3: `WithOrphanCleanup`
- [x] T4: Windows: keep the Job Object open while it has members
- [x] T5: `StopAll` selects by state, not by operating-system liveness
- [x] T6: Document the pipe interaction, `WaitDelay`, and the new behaviour

## Problem / Goal

With `WithProcessTreeTermination`, procman cleaned up descendants only while a stop was in
progress. When the direct child exited on its own, whatever it left in its process group kept
running, and nothing in procman would ever signal it again:

- `Stop`, `StopAll` and `Shutdown` skipped commands that were no longer running.
- Since #11, `KillAll` skipped them too. Before #11 it sent `kill(-pid, SIGKILL)` to every
  command ever started. That killed leftovers whose group still existed, but risked SIGKILLing an
  unrelated group that had reused the ID. It could not tell the two apart.

A related gap: when `Stdout`/`Stderr` are writers, `exec.Cmd.Wait` returns only once every
holder of the output pipes has closed them. A direct child that died while descendants held its
pipes was reported running by its status but dead by `IsRunning`. `StopAll` selected by
`IsRunning` and skipped it, and `Shutdown` then waited on `Done` for as long as the descendants
lived.

Success looks like: a consumer can learn when a command's whole tree is gone, can stop leftovers
at any point until then, and can ask for them to be stopped automatically. None of this ever
signals an identifier that may have been reused.

## Goals

- Leftover descendants stay reachable from the moment the direct child is reaped until the tree
  is first observed empty.
- Their end is observable as a channel.
- Opt-in automatic cleanup of leftovers when the direct child exits on its own.
- No signal to a tree after it has been observed empty.

## Non-goals

- **Descendants that leave the tree** (`setsid`, `setpgid`, Job Object breakaway). They are
  outside procman's reach, as before.
- **Event-driven tracking.** Neither platform offers an exit notification for a process group or
  job that procman is not the parent of, so tracking polls.
- **The Windows creation-to-assignment window.** A child can start processes before it is
  assigned to its Job Object. Closing it needs `CREATE_SUSPENDED` and a resume, which `os/exec`
  does not expose. It is documented instead.
- **Non-tree commands whose pipes are held by processes procman cannot reach.** `Done` still
  waits for them; `exec.Cmd.WaitDelay` is the caller's tool, and the README now says so.

## Proposed Design

### Tracking

The reaper checks the tree once `cmd.Wait` returns, before `Done` closes. If members remain, it
sets `treeTracked` and, after closing `Done`, stays on as the tracker. It checks the tree every
50ms until it is first seen empty, then clears `treeTracked`, releases platform state and closes
`treeDone`. An empty process group or Job Object cannot gain members, so that observation is
final.

- Unix: `kill(-pgid, 0)`. While the group has a member, POSIX does not reuse its ID.
- Windows: `ActiveProcesses` from `JobObjectBasicAccountingInformation`. A job handle cannot be
  reused while procman holds it.

### Signalling a tracked tree

`signalTrackedTree` acts only while `treeTracked` is set, and checks for members again
immediately before signalling, under the same lock the tracker uses to end tracking. The
identifier it signals has therefore just been seen in use. This is the freshness rule `Stop`
introduced in #11, now shared by every path.

### Who reaches it

| Path | Running command | Tracked leftovers |
|---|---|---|
| `Stop` | as before; with grace > 0, waits for `TreeDone` | graceful signal, grace period, then kill (Unix); immediate termination (Windows) |
| `StopAll` / `Shutdown` | as before | as `Stop` |
| `KillAll` | as before | immediate kill |
| `WithOrphanCleanup` | n/a | as `Stop`, started by the reaper when the exit was not requested |

`StopRequested` is not set when only leftovers are stopped: the command itself exited on its own.

A zero grace period never waits for descendants, consistent with #11.

### Windows

The Job Object stays open while it has members, and only with process-tree termination. With
parent-death cleanup alone, the reaper closes it at once as before, and kill-on-close ends any
descendants. Leftovers get no graceful signal: `CTRL_BREAK_EVENT` addresses a process group by
its leader's PID, which Windows may already have reused.

## Affected Modules

- `procman.go`: tracking, `TreeDone`, `WithOrphanCleanup`, `Stop`/`StopAll`/`KillAll` changes.
- `procman_unix.go`, `procman_windows.go`: `treeAlive`, `signalTree`, `gracefulTreeSignal`.
- `orphan_tracking_test.go`: portable behaviour, using a test-binary helper that leaves a
  descendant behind.
- `orphan_policy_unix_test.go`, `procman_unix_internal_test.go`: SIGTERM-ignoring descendants,
  the pipe case, and the tracked-only signalling gate.
- `README.md`, `doc.go`.

## Tasks

### T1: Track a populated tree after its direct child is reaped; expose `TreeDone`

**Outcome**: `TreeDone()` closes after `Done`, once the tree is first seen empty. Without
process-tree termination, it closes right after `Done`.

**Verification**: `TestTreeDoneWaitsForDescendantsAndStopEndsThem`,
`TestTreeDoneFollowsDoneWithoutProcessTreeTermination`.

**Notes.** Done. The reaper goroutine doubles as the tracker, so tracking costs no extra goroutine.
The tracking check happens before `Done` closes, so there is no moment at which an observer has
seen the exit but the leftovers are not yet tracked.

### T2: Let `Stop`, `StopAll`, `KillAll` and `Shutdown` reach tracked descendants

**Outcome**: each stops or kills leftovers while the tree is tracked.

**Verification**: `TestStopWithZeroGraceEndsTrackedDescendants`,
`TestKillAllKillsTrackedDescendants`, `TestShutdownEndsTrackedDescendants`, and
`TestSignalTrackedTreeOnlySignalsTrackedTrees`, which points a command at an unrelated live
process group and checks that only a tracked tree is signalled.

**Notes.** Done. `Stop`'s own polling from #11 (`awaitTreeExit`) is replaced by waiting on
`TreeDone`, so there is a single tracker per command. #11's `TestKillAllSkipsReapedCommands`
pinned the behaviour this issue reverses, and is removed.

### T3: `WithOrphanCleanup`

**Outcome**: when the direct child exits on its own, leftovers are stopped as `Stop` would. The
option requires `WithProcessTreeTermination`; command creation fails without it.

**Verification**: `TestOrphanCleanupEndsDescendantsWithoutStop`,
`TestOrphanCleanupKillsDescendantsThatIgnoreSIGTERM`,
`TestOrphanCleanupRequiresProcessTreeTermination`.

### T4: Windows: keep the Job Object open while it has members

**Outcome**: with process-tree termination, the job is closed when the tree empties instead of
when the direct child is reaped.

**Verification**: the portable tests in `orphan_tracking_test.go` run on Windows CI.

**Notes.** Done. `JOBOBJECT_BASIC_ACCOUNTING_INFORMATION` is not in `golang.org/x/sys/windows`
and is mirrored locally. Behaviour change: with process-tree termination *and* parent-death
cleanup, leftovers used to be killed when the job closed at reap time. They are now tracked like
on Unix; `WithOrphanCleanup` restores automatic termination.

### T5: `StopAll` selects by state, not by operating-system liveness

**Outcome**: a command whose direct child has died while `Wait` drains its pipes is stopped.

**Verification**: `TestShutdownReachesDescendantsHoldingOutputPipes`. It fails within its 5s
bound when `StopAll` selects by `IsRunning`.

### T6: Document the pipe interaction, `WaitDelay`, and the new behaviour

**Outcome**: README sections on descendants that outlive the command and on output pipes, updated
limitations, and a `doc.go` section.
