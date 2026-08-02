//go:build !windows

package procman_test

// processAliveWindows is unused on non-Windows; provided so the untagged
// helper compiles.
func processAliveWindows(pid int) bool { return false }