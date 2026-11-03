// -------------------------------------------------------------------------------
// Instance Identity
//
// Author: Alex Freidah
//
// A stable per-process identifier, hostname-XXXXXXXX, telling instances apart
// in shared state, cleanup claims and logs. No claim depends on it for
// correctness.
// -------------------------------------------------------------------------------

package instanceid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

// ID is the resolved instance identifier in the form hostname-XXXXXXXX.
type ID string

// String returns the identifier as a plain string for stamping into SQL
// parameters and log/audit fields.
func (i ID) String() string { return string(i) }

// New builds a fresh instance identifier from the hostname ("unknown" if it
// cannot be read) and an 8-hex crypto/rand suffix, so two processes on the
// same host get distinct identifiers.
func New() ID {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}

	// rand.Read never returns an error: it crashes the process instead.
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	return ID(fmt.Sprintf("%s-%s", host, hex.EncodeToString(suffix[:])))
}
