// -------------------------------------------------------------------------------
// Import Classification
//
// Author: Alex Freidah
//
// Decides what encryption metadata a discovered backend object should be
// imported with. Import is the one write path that starts from bytes rather
// than from a client request, so it is the only place the orchestrator has to
// infer encryption state instead of being told it.
// -------------------------------------------------------------------------------

package core

import "github.com/afreidah/s3-orchestrator/internal/encryption"

// ImportDecision is what ClassifyImport concluded about discovered bytes.
type ImportDecision int

const (
	// ImportPlaintext means the bytes carry no envelope and are the object.
	ImportPlaintext ImportDecision = iota

	// ImportAdoptKey means the bytes are an envelope produced by the same
	// encryption run as an existing row for this key, so that row's key
	// reads them.
	ImportAdoptKey

	// ImportUnreadable means the bytes are an envelope no known key opens.
	// The object is recorded so its space is accounted for, but nothing can
	// serve it.
	ImportUnreadable
)

// String renders the decision for logs and audit lines.
func (d ImportDecision) String() string {
	switch d {
	case ImportPlaintext:
		return "plaintext"
	case ImportAdoptKey:
		return "adopted_key"
	case ImportUnreadable:
		return "unreadable"
	default:
		return "unknown"
	}
}

// ClassifyImport decides how to record bytes discovered on a backend, given
// the object's envelope header and the rows the ledger already holds for that
// key on other backends.
//
// Adoption is deliberately not granted on a key-name match alone. Every PUT
// mints a fresh DEK, so a stray copy of a key is usually an earlier write
// whose key died with its row; handing it a sibling's key would produce a row
// that claims to be readable and is not. The header's base nonce is what
// distinguishes the two, since it is unique per encryption run and copies
// reproduce it byte for byte.
//
// A nil return means record no encryption metadata at all.
func ClassifyImport(header []byte, siblings []ObjectLocation) (ImportDecision, *EncryptionMeta) {
	if !encryption.HasEnvelopeMagic(header) {
		return ImportPlaintext, nil
	}
	for i := range siblings {
		s := &siblings[i]
		if !s.Encrypted || len(s.EncryptionKey) == 0 {
			continue
		}
		if !encryption.SameEncryptionOperation(header, s.EncryptionKey) {
			continue
		}
		return ImportAdoptKey, &EncryptionMeta{
			Encrypted:     true,
			EncryptionKey: s.EncryptionKey,
			KeyID:         s.KeyID,
			PlaintextSize: s.PlaintextSize,
			ContentHash:   s.ContentHash,
		}
	}
	// Recording this as plaintext is what publishes ciphertext to clients as
	// though it were the object, so an envelope with no matching key is
	// recorded as encrypted-and-keyless instead: unreadable, but honest, and
	// the read path refuses it rather than serving it.
	return ImportUnreadable, &EncryptionMeta{Encrypted: true}
}
