//go:build !linux

package procman_test

// linuxProcState is a no-op stub on non-Linux (no /proc).
func linuxProcState(pid int) string { return "" }