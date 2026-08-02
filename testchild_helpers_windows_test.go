//go:build windows

package procman_test

import "syscall"

// processAliveWindows reports whether a PID is alive on Windows via
// OpenProcess with the SYNCHRONIZE right and WaitForSingleObject-style check.
// Simpler: OpenProcess returns 0 if the process does not exist or we lack
// access (a still-alive process we own is accessible).
func processAliveWindows(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	openProcess := kernel32.NewProc("OpenProcess")
	h, _, _ := openProcess.Call(uintptr(processQueryLimitedInformation), 0, uintptr(pid))
	if h == 0 {
		return false
	}
	syscall.CloseHandle(syscall.Handle(h))
	return true
}