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
type ObjectLocation struct {
	Backend       string    `json:"backend"`
	SizeBytes     int64     `json:"size_bytes"`
	CreatedAt     time.Time `json:"created_at"`
	Encrypted     bool      `json:"encrypted"`
	KeyID         string    `json:"key_id,omitempty"`
	PlaintextSize int64     `json:"plaintext_size"`
	ContentHash   string    `json:"content_hash,omitempty"`
}
