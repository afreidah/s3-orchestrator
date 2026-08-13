// -------------------------------------------------------------------------------
// Admin API - Shared Object-Location DTOs
//
// Author: Alex Freidah
//
// Wire types for the per-object location ledger shared by the admin
// object-locations handler and its clients (the TUI inspector, adminctl). Kept
// in the leaf adminapi package so the server and out-of-process clients depend
// on one definition and the JSON shape cannot drift.
// -------------------------------------------------------------------------------

package adminapi

import "time"

// ObjectLocationsResponse lists every backend copy of a single object key.
type ObjectLocationsResponse struct {
	Key       string           `json:"key"`
	Locations []ObjectLocation `json:"locations"`
}

// ObjectLocation is one backend copy of an object. The raw envelope encryption
// key is deliberately omitted from the wire; only the encrypted flag and the
// key id are exposed so a secret never leaves the process.
//
// LastScrubbedAt is absent for a copy that has never been verified rather than
// zero-valued, because "never checked" and "checked at the epoch" are different
// answers to the question this field exists for. Having a ContentHash only says
// a hash was recorded; this says whether the bytes were ever compared to it.
type ObjectLocation struct {
	Backend        string     `json:"backend"`
	SizeBytes      int64      `json:"size_bytes"`
	CreatedAt      time.Time  `json:"created_at"`
	Encrypted      bool       `json:"encrypted"`
	KeyID          string     `json:"key_id,omitempty"`
	PlaintextSize  int64      `json:"plaintext_size"`
	ContentHash    string     `json:"content_hash,omitempty"`
	LastScrubbedAt *time.Time `json:"last_scrubbed_at,omitempty"`
}
