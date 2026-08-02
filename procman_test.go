package procman_test

import (
	"testing"

	procman "github.com/mainvec/procman"
)

// TestID is a placeholder smoke test retained from the scaffold rename. It
// verifies the package builds and that the retained ID type still works; it
// is replaced by the T3 suite once Supervisor lands.
func TestID(t *testing.T) {
	id := procman.NewID()
	if id.String() == "" {
		t.Fatal("expected a non-empty ID string")
	}
}
