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

// ImportPlaintext and the other classifications of discovered bytes.
// AdoptKey means the envelope came from the same encryption run as an existing
// row for the key. Unreadable objects are still recorded because their space is
// real. Compressed is recognised by the seek table, which a plain zstd encoder
// never writes.
const (
	ImportPlaintext  ImportDecision = iota // no envelope; the bytes are the object
	ImportAdoptKey                         // envelope an existing row's key opens
	ImportUnreadable                       // envelope no known key opens
	ImportCompressed                       // an encoding this orchestrator can decode
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

// -------------------------------------------------------------------------
// DISCOVERED BYTES
// -------------------------------------------------------------------------

// DiscoveredBytes is what the reconciler learned about a rediscovered object
// without decoding it: its header and what the codec made of the stored form.
// Compressed and LogicalSize are only consulted for non-envelope bytes, since
// compression runs before encryption and is hidden inside the ciphertext.
type DiscoveredBytes struct {
	Header      []byte
	Compressed  bool
	LogicalSize int64
}

// -------------------------------------------------------------------------
// CLASSIFICATION
// -------------------------------------------------------------------------

// ClassifyImport decides how to record bytes discovered on a backend, given
// their header and the key's rows on other backends. A nil form means record
// no representation metadata.
//
// A key-name match alone does not grant adoption: every PUT mints a fresh DEK,
// so a stray copy is usually an earlier write whose key died with its row. A
// sibling is adopted only when its base nonce, unique per encryption run,
// matches the header, and then its whole description is adopted.
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
