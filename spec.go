package procman

import (
	"errors"
	"io"
	"time"
)

// State is the lifecycle state of a Process.
type State int

const (
	StateStarting State = iota
	StateRunning
	StateStopping
	StateRestarting
	StateExited
)

// String returns a human-readable state name.
func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateStopping:
		return "stopping"
	case StateRestarting:
		return "restarting"
	case StateExited:
		return "exited"
	default:
		return "unknown"
	}
}

// Stream identifies which child output stream a Line came from.
type Stream int

const (
	StreamStdout Stream = iota
	StreamStderr
)

// String returns "stdout" or "stderr".
func (s Stream) String() string {
	if s == StreamStderr {
		return "stderr"
	}
	return "stdout"
}

// RestartMode selects when a process is restarted after an exit.
type RestartMode int

const (
	// RestartNever never restarts; the first exit is terminal.
	RestartNever RestartMode = iota
	// RestartOnFailure restarts only on a non-zero exit or signal death.
	RestartOnFailure
	// RestartAlways restarts on any exit, including a successful one.
	RestartAlways
)

// RestartPolicy governs restart behaviour and backoff. It is honoured on
// non-final exits; a caller-initiated Stop is never treated as a failure.
type RestartPolicy struct {
	Mode         RestartMode
	InitialDelay time.Duration
	MaxDelay     time.Duration
	Multiplier   float64
	MaxRetries   int           // 0 means unlimited
	ResetAfter   time.Duration // uptime after which the retry counter resets
}

// Watchdog selects the parent-death guarantee mode.
type Watchdog int

const (
	// WatchdogAuto enables the watchdog where the platform supports it (Unix
	// sidecar; Windows uses a Job Object regardless). Default.
	WatchdogAuto Watchdog = iota
	// WatchdogOff disables the parent-death guarantee entirely.
	WatchdogOff
)

// Options configures a Supervisor. The zero value is not usable; construct
// via New.
type Options struct {
	Watchdog Watchdog
	// ShellPath overrides the sidecar shell used by the Unix watchdog; ""
	// means /bin/sh.
	ShellPath string
	// Logger receives structured lifecycle events; nil means no logging.
	Logger io.Writer
	// OnExit is invoked once per generation when a process exits. It runs on
	// the reaper goroutine and must not block.
	OnExit func(*Process, ExitInfo)
}

// Spec describes a process to start. Name is the registry key and must be
// unique within a Supervisor. Path is the executable; Args is argv[1:].
type Spec struct {
	Name      string
	Path      string
	Args      []string
	Env       []string
	Dir       string
	StopGrace time.Duration
	Restart   RestartPolicy

	// Stdout and Stderr are optional byte sinks written as data arrives.
	Stdout, Stderr io.Writer
	// OnLine is an optional per-line callback for both streams.
	OnLine func(Line)
	// LogTailLines, when > 0, keeps the last N lines in an in-memory ring.
	LogTailLines int
}

// ExitInfo is the result of a process generation's exit.
type ExitInfo struct {
	Code       int
	Signal     string    // non-empty if killed by a signal (Unix)
	ExitedAt   time.Time
	Generation int
	Err        error // ErrRestartBudgetExhausted, or a supervision failure
}

// Line is a single captured output line from a child stream.
type Line struct {
	Stream Stream
	Text   string
	At     time.Time
}

// StartError is returned by Supervisor.Start when the child could not be
// started or started and died immediately. It distinguishes a pure exec
// failure (Exit is nil) from an immediate death (Exit is set).
type StartError struct {
	Spec    Spec
	Exit    *ExitInfo // set when the child started and then died immediately
	LogTail []Line
	Err     error
}

func (e *StartError) Error() string {
	if e.Err == nil {
		return "procman: start error"
	}
	return "procman: " + e.Err.Error()
}

func (e *StartError) Unwrap() error { return e.Err }

// Sentinel errors.
var (
	ErrNotRunning        = errors.New("procman: process not running")
	ErrAlreadyRunning    = errors.New("procman: process already running")
	ErrDuplicateName     = errors.New("procman: duplicate process name")
	ErrRestartBudgetExhausted = errors.New("procman: restart budget exhausted")
)