// Package procman is a zero-dependency, cross-platform Go library for robust
// process supervision.
//
// It exists to close the three gaps that os/exec leaves open: children that
// outlive their parent, exits that go unobserved, and output that is
// unrecoverable when a child dies during startup.
//
// # Two invariants
//
// Everything in the package holds one of two lines:
//
//  1. A child never outlives its parent — including SIGKILL of the parent.
//  2. The parent always learns that a child died — exactly once, via a single
//     reaper goroutine per generation that owns cmd.Wait.
//
// # Platform table
//
// The strongest available primitive differs by platform, so the mechanism does
// too:
//
//	|           | Group isolation | Graceful stop      | Hard kill            | Parent-death guarantee                |
//	|-----------|-----------------|--------------------|----------------------|---------------------------------------|
//	| Linux     | Setpgid         | kill(-pgid,SIGTERM)| kill(-pgid,SIGKILL)  | /bin/sh sidecar watchdog              |
//	| macOS     | Setpgid         | kill(-pgid,SIGTERM)| kill(-pgid,SIGKILL)  | /bin/sh sidecar watchdog              |
//	| Windows   | Job Object      | TerminateJobObject | TerminateJobObject   | JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE    |
//
// Windows needs no watchdog: the parent holds the only handle to a Job Object
// that kills its contents when the handle closes, which happens on process
// death by any means. Unix has no equivalent kernel mechanism, so a /bin/sh
// sidecar blocks on a sentinel pipe and kills the target's process group on
// EOF. See the design in plans/001-process-supervision-library.md.
//
// # Limitations
//
// These are documented rather than hidden:
//
//   - The Unix watchdog can be killed independently by PID; it is a process,
//     not a kernel guarantee.
//   - A supervised child is a process-group leader (Setpgid), so a direct
//     setsid() from it fails with EPERM and it cannot escape. A grandchild
//     that calls setsid() is not a leader and escapes its process group on
//     Unix; procman does not track descendants that detach.
//   - Options.Watchdog set to WatchdogOff opts out of the parent-death
//     guarantee entirely.
//   - On Windows there is a brief unprotected window between cmd.Start and
//     AssignProcessToJobObject; Go exposes no CREATE_SUSPENDED path.
//   - stdout and stderr are not ordered relative to each other; this is
//     inherent to two pipes.
//   - On a system with no /bin/sh the fallback re-executes the host binary and
//     its init() hook runs before main, so a host with expensive package-level
//     initialisation pays that cost once per spawn.
package procman