package procman

import (
	"flag"
	"os"
	"testing"
)

// TestMain dispatches the self re-exec test modes. When the test binary is
// re-executed with PROCMAN_TEST_CHILD=1 (the child harness) or
// PROCMAN_TEST_PARENT=1 (the out-of-process parent used by T10), it runs that
// behaviour and exits before any test runs. Otherwise it runs the normal test
// suite.
func TestMain(m *testing.M) {
	if TryRunTestChild() {
		// Never returns: the child exited.
		os.Exit(0)
	}
	if TryRunTestParent() {
		// Never returns: the parent blocks until killed.
		os.Exit(0)
	}
	flag.Parse()
	os.Exit(m.Run())
}