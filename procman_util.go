package procman

import (
	"crypto/rand"
	"fmt"
)

// ID uniquely identifies a command within a Procman registry.
type ID [16]byte

// NewID returns a cryptographically random command identifier. It panics only
// if the operating system's secure random source fails.
func NewID() ID {
	var id ID
	_, err := rand.Read(id[:])
	if err != nil {
		panic(err) // rand.Read should never fail
	}
	return id
}

// String returns the identifier in a UUID-style hexadecimal representation.
func (id ID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}

// Args returns its arguments as a string slice for use with NewExecCmd.
func Args(args ...string) []string {
	return args
}
