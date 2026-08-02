package procman

import (
	"flag"
	"os"
	"testing"
)

// TestMain dispatches the self re-exec test child. When the test binary is
// re-executed with PROCMAN_TEST_CHILD=1 (see testchild.go), it runs the child
// behaviour and exits before any test runs. Otherwise it runs the normal test
// suite.
func TestMain(m *testing.M) {
	if TryRunTestChild() {
		// Never returns: the child exited.
		os.Exit(0)
	}
	flag.Parse()
	os.Exit(m.Run())
}