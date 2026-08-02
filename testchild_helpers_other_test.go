//go:build !unix

package procman_test

import "os"

// syscallKillGroupUnix is a no-op on non-Unix (no signal-based groups).
func syscallKillGroupUnix(pgid, sig int) error {
	return os.ErrNotExist
}