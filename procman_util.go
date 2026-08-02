package procman

import (
	"crypto/rand"
	"fmt"
)

type ID [16]byte // Same size as UUID v4

func NewID() ID {
	var id ID
	_, err := rand.Read(id[:])
	if err != nil {
		panic(err) // rand.Read should never fail
	}
	return id
}

func (id ID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}
