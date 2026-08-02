//go:build linux

package procman_test

import "os"

// linuxProcState reads /proc/<pid>/stat and returns the single-character
// process state (e.g. "R", "Z"). Returns "" if it cannot be read.
func linuxProcState(pid int) string {
	b, err := os.ReadFile("/proc/" + itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	// /proc/<pid>/stat: pid (comm) state ...
	// comm may contain spaces and parens; find the last ')' and read past it.
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == ')' {
			if i+2 < len(b) {
				return string(b[i+2])
			}
			return ""
		}
	}
	return ""
}