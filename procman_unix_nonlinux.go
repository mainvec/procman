//go:build unix && !linux

package procman

// SupportsParentDeathCleanup reports whether native parent-death cleanup is
// available on the current platform.
func SupportsParentDeathCleanup() bool {
	return false
}

func configureParentDeathSignal(child *ExecCmd) error {
	return nil
}
