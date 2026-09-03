// -------------------------------------------------------------------------------
// Instance Identity
//
// Author: Alex Freidah
//
// Resolves a stable per-process identifier used to stamp the cleanup_queue
// claimed_by column for observability and the cleanup_queue.claim_recovered
// audit event. The identifier has no role in correctness decisions  -  the
// claim race is settled by FOR UPDATE SKIP LOCKED in postgres and by SQLite's
// single-writer model. The format hostname-XXXXXXXX (8 lowercase hex chars)
// keeps a human-readable hostname prefix while still differentiating two
// instances on the same host (e.g., a rolling deploy with overlap, or two
// pods on the same node).
//
// The identifier is computed once at construction (DI singleton) so every
// claim within a process carries the same value; future-instance comparisons
// across pods stay meaningful.
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

// New builds a fresh instance identifier. The hostname is taken from
// os.Hostname() and falls back to "unknown" when the OS call fails (rare,
// but the identifier is observability-only so a soft fallback is safer than
// a startup failure). The 8-hex suffix comes from crypto/rand so two
// processes on the same host get distinct identifiers; if rand.Read ever
// fails the function returns the error - the caller decides whether to
// fail-fast or carry on with a non-unique identifier.
func New() (ID, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}

	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("instanceid: read random suffix: %w", err)
	}
	return ID(fmt.Sprintf("%s-%s", host, hex.EncodeToString(suffix[:]))), nil
}
