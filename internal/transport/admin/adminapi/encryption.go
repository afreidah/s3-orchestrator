// -------------------------------------------------------------------------------
// Admin API - Shared Bulk Encryption DTOs
//
// Author: Alex Freidah
//
// Wire types for the bulk encryption endpoints (key rotation, encrypt-existing,
// decrypt-existing) shared by the handler and its clients. Kept in the leaf
// adminapi package so the server and its out-of-process client depend on one
// definition and the JSON shape cannot drift.
// -------------------------------------------------------------------------------

package adminapi

// BulkEncryptionOutcome is the part every bulk encryption pass reports
// identically: the terminal status plus the failure and attempt counts. The
// per-operation responses embed it and add their own success count.
//
// The three operations name that success count differently on the wire
// (rotated, encrypted, decrypted) even though each reports the same quantity.
// The names are kept because they are already published; sharing the rest of
// the shape is what stops the three from drifting apart further.
type BulkEncryptionOutcome struct {
	Status string `json:"status"`
	Failed int    `json:"failed"`
	Total  int    `json:"total"`
}

// RotateEncryptionKeyResponse reports a key-rotation pass: how many DEKs were
// re-wrapped under the current primary key.
type RotateEncryptionKeyResponse struct {
	BulkEncryptionOutcome
	Rotated int `json:"rotated"`
}

// EncryptExistingResponse reports an encrypt-existing pass: how many
// previously plaintext objects were encrypted in place.
type EncryptExistingResponse struct {
	BulkEncryptionOutcome
	Encrypted int `json:"encrypted"`
}

// DecryptExistingResponse reports a decrypt-existing pass: how many encrypted
// objects were rewritten back to plaintext.
type DecryptExistingResponse struct {
	BulkEncryptionOutcome
	Decrypted int `json:"decrypted"`
}
