// -------------------------------------------------------------------------------
// Import Classification
//
// Author: Alex Freidah
//
// Decides what representation metadata a discovered backend object should be
// imported with. Import is the one write path that starts from bytes rather
// than from a client request, so it is the only place the orchestrator has to
// infer how an object is stored instead of being told it.
// -------------------------------------------------------------------------------

package core

import (
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

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

	// ImportCompressed means the bytes are an encoding this orchestrator can
	// decode, recognised by the seek table a plain zstd encoder never writes.
	ImportCompressed
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
	case ImportCompressed:
		return "compressed"
	default:
		return "unknown"
	}
}

// DiscoveredBytes is everything the reconciler could learn about a rediscovered
// object without decoding it: the head, where an encryption envelope announces
// itself, and what the codec made of the stored form.
//
// Compressed and LogicalSize are only consulted for bytes that are not an
// envelope. Compression runs before encryption, so an encrypted object's
// encoding is inside the ciphertext and invisible from here; that case is
// covered by adopting a sibling's description instead.
type DiscoveredBytes struct {
	Header      []byte
	Compressed  bool
	LogicalSize int64
}

// ClassifyImport decides how to record bytes discovered on a backend, given
// what could be learned from the bytes and the rows the ledger already holds
// for that key on other backends.
//
// Adoption is deliberately not granted on a key-name match alone. Every PUT
// mints a fresh DEK, so a stray copy of a key is usually an earlier write
// whose key died with its row; handing it a sibling's key would produce a row
// that claims to be readable and is not. The header's base nonce is what
// distinguishes the two, since it is unique per encryption run and copies
// reproduce it byte for byte. A sibling that matches describes the same stored
// bytes in every respect, so its whole description is adopted rather than its
// key alone.
//
// A nil return means record no representation metadata at all.
func ClassifyImport(b DiscoveredBytes, siblings []ObjectLocation) (ImportDecision, *StoredForm) {
	if !encryption.HasEnvelopeMagic(b.Header) {
		if b.Compressed {
			// No level: the encoding does not record it and decoding does not
			// need it. Only a rewrite pass reads that column, and it can treat
			// an unknown level as the configured one.
			return ImportCompressed, &StoredForm{
				CompressionAlgorithm:     compression.Algorithm,
				CompressionFormatVersion: compression.FormatVersion,
				LogicalSize:              b.LogicalSize,
			}
		}
		return ImportPlaintext, nil
	}
	for i := range siblings {
		s := &siblings[i]
		if !s.Encrypted || len(s.EncryptionKey) == 0 {
			continue
		}
		if !encryption.SameEncryptionOperation(b.Header, s.EncryptionKey) {
			continue
		}
		return ImportAdoptKey, StoredFormFromLocation(s)
	}
	// Recording this as plaintext is what publishes ciphertext to clients as
	// though it were the object, so an envelope with no matching key is
	// recorded as encrypted-and-keyless instead: unreadable, but honest, and
	// the read path refuses it rather than serving it.
	return ImportUnreadable, &StoredForm{Encrypted: true}
}
