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

// RotateEncryptionKeyResponse reports a key-rotation pass: how many DEKs were
// re-wrapped under the current primary key.
//
// Rotation does not embed BulkRewriteOutcome. It re-wraps a DEK without reading
// or writing the object's bytes, so it is not a rewrite pass and has nothing a
// skipped count would describe.
type RotateEncryptionKeyResponse struct {
	Status  string `json:"status"`
	Failed  int    `json:"failed"`
	Total   int    `json:"total"`
	Rotated int    `json:"rotated"`
}

// EncryptExistingResponse reports an encrypt-existing pass: how many
// previously plaintext objects were encrypted in place.
type EncryptExistingResponse struct {
	BulkRewriteOutcome
	Encrypted int `json:"encrypted"`
}

// DecryptExistingResponse reports a decrypt-existing pass: how many encrypted
// objects were rewritten back to plaintext.
type DecryptExistingResponse struct {
	BulkRewriteOutcome
	Decrypted int `json:"decrypted"`
}

// RotateEncryptionKeyRequest is the body of a key-rotation call: the key ID
// whose sealed DEKs should be re-wrapped under the current primary key.
type RotateEncryptionKeyRequest struct {
	OldKeyID string `json:"old_key_id"`
}
