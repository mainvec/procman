package procman

import (
	"crypto/rand"
	"fmt"
)

// ID is a 16-byte identifier (UUID v4 layout without the version/variant bits
// enforced). It is the registry key for processes in a Supervisor.
type ID [16]byte

// NewID returns a random ID. It panics if the system CSPRNG fails, which
// should never happen on a supported platform.
func NewID() ID {
	var id ID
	if _, err := rand.Read(id[:]); err != nil {
		panic(err) // rand.Read should never fail
	}
	return id
}

// String returns a hyphenated hex representation, for logs and diagnostics.
func (id ID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}