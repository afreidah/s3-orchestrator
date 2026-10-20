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
// re-wrapped under the current primary key. It does not embed
// BulkRewriteOutcome because rotation never touches the object's bytes.
type RotateEncryptionKeyResponse struct {
	Status  string `json:"status"`
	Failed  int    `json:"failed"`
	Total   int    `json:"total"`
	Rotated int    `json:"rotated"`
}

// EncryptExistingResponse reports an encrypt-existing pass: how many
// previously plaintext objects were encrypted on their own backend.
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
// whose sealed DEKs should be re-wrapped under the current primary key, or
// AllStale to re-wrap every DEK not already under it. Exactly one is set.
type RotateEncryptionKeyRequest struct {
	OldKeyID string `json:"old_key_id,omitempty"`
	AllStale bool   `json:"all_stale,omitempty"`
}
