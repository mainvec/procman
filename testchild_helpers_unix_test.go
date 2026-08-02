//go:build unix

package procman_test

import "syscall"

// syscallKillGroupUnix sends a signal to the negative process group.
func syscallKillGroupUnix(pgid, sig int) error {
	return syscall.Kill(-pgid, syscall.Signal(sig))
}