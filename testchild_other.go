//go:build !unix

package procman

import "os/exec"

func testChildSetsid() error {
	// setsid is a Unix concept; on Windows there is no process-group escape
	// to model. Report unsupported so a test that asks for it is skipped on
	// non-Unix runners rather than silently passing.
	return errSetsidUnsupported
}

var errSetsidUnsupported = &setsidUnsupportedError{}

type setsidUnsupportedError struct{}

func (e *setsidUnsupportedError) Error() string { return "setsid not supported on this platform" }

func ignoreSIGTERM() {
	// SIGTERM is not a Windows concept; TerminateProcess is unconditional.
	// No-op so the flag is harmless on Windows, but tests that depend on
	// escalation semantics are skipped there.
}

func prepareGrandchildCmd(cmd *exec.Cmd) {
	// No process-group attributes on Windows; Job Object membership is set
	// by the supervisor, not here.
}