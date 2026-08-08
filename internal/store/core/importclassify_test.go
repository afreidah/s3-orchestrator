// -------------------------------------------------------------------------------
// Import Classification Tests
//
// Author: Alex Freidah
//
// Covers the decision that keeps rediscovered ciphertext from being recorded
// as plaintext: adoption is granted only to a sibling row whose key provably
// encrypted the bytes, never on a matching key name alone.
// -------------------------------------------------------------------------------

package core

import (
	"bytes"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// envelopeHeader builds a valid 32-byte envelope header carrying baseNonce.
func envelopeHeader(baseNonce []byte) []byte {
	hdr := make([]byte, encryption.HeaderSize)
	copy(hdr, "SENC")
	hdr[4] = 0x01                                  // version
	copy(hdr[5:9], []byte{0x00, 0x01, 0x00, 0x00}) // chunk size 65536
	copy(hdr[9:21], baseNonce)
	return hdr
}

// nonce builds a distinguishable 12-byte base nonce.
func nonce(fill byte) []byte { return bytes.Repeat([]byte{fill}, encryption.NonceSize) }

// encryptedSibling builds a location row holding a packed key for baseNonce.
func encryptedSibling(backendName string, baseNonce []byte) ObjectLocation {
	return ObjectLocation{
		BackendName:   backendName,
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(baseNonce, []byte("wrapped-dek")),
		KeyID:         "key-1",
		PlaintextSize: 1024,
		ContentHash:   "abc123",
	}
}

// TestClassifyImport_Plaintext verifies bytes with no envelope are imported
// with no encryption metadata, whatever the ledger holds for the key.
func TestClassifyImport_Plaintext(t *testing.T) {
	t.Parallel()
	siblings := []ObjectLocation{encryptedSibling("b1", nonce(0xAA))}
	decision, enc := ClassifyImport([]byte("just a plain object body"), siblings)
	if decision != ImportPlaintext {
		t.Errorf("decision = %v, want ImportPlaintext", decision)
	}
	if enc != nil {
		t.Errorf("plaintext must carry no encryption metadata, got %+v", enc)
	}
}

// TestClassifyImport_AdoptsMatchingSibling verifies a sibling whose stored key
// was produced by the same encryption run is adopted in full.
func TestClassifyImport_AdoptsMatchingSibling(t *testing.T) {
	t.Parallel()
	n := nonce(0xAA)
	sib := encryptedSibling("b1", n)
	decision, enc := ClassifyImport(envelopeHeader(n), []ObjectLocation{sib})
	if decision != ImportAdoptKey {
		t.Fatalf("decision = %v, want ImportAdoptKey", decision)
	}
	if !enc.Encrypted || !bytes.Equal(enc.EncryptionKey, sib.EncryptionKey) {
		t.Errorf("key not adopted: %+v", enc)
	}
	if enc.KeyID != "key-1" || enc.PlaintextSize != 1024 || enc.ContentHash != "abc123" {
		t.Errorf("sibling metadata not carried over: %+v", enc)
	}
}

// TestClassifyImport_RefusesSiblingFromDifferentWrite is the case the whole
// check exists for: the key name matches but the bytes came from an earlier
// write whose DEK is gone, so adopting would claim a readable object that
// cannot be decrypted.
func TestClassifyImport_RefusesSiblingFromDifferentWrite(t *testing.T) {
	t.Parallel()
	sib := encryptedSibling("b1", nonce(0xAA))
	decision, enc := ClassifyImport(envelopeHeader(nonce(0xBB)), []ObjectLocation{sib})
	if decision != ImportUnreadable {
		t.Fatalf("decision = %v, want ImportUnreadable", decision)
	}
	if !enc.Encrypted {
		t.Error("an envelope must never be recorded as plaintext")
	}
	if len(enc.EncryptionKey) != 0 {
		t.Error("a key from a different write must not be adopted")
	}
}

// TestClassifyImport_NoSibling verifies a genuine orphan envelope is recorded
// as encrypted and keyless rather than as plaintext.
func TestClassifyImport_NoSibling(t *testing.T) {
	t.Parallel()
	decision, enc := ClassifyImport(envelopeHeader(nonce(0xAA)), nil)
	if decision != ImportUnreadable {
		t.Fatalf("decision = %v, want ImportUnreadable", decision)
	}
	if !enc.Encrypted || len(enc.EncryptionKey) != 0 {
		t.Errorf("want encrypted with no key, got %+v", enc)
	}
}

// TestClassifyImport_SkipsUnusableSiblings verifies rows that cannot supply a
// key are passed over rather than adopted from: a plaintext row, and a row
// flagged encrypted whose own key is already gone.
func TestClassifyImport_SkipsUnusableSiblings(t *testing.T) {
	t.Parallel()
	n := nonce(0xAA)
	siblings := []ObjectLocation{
		{BackendName: "b1"},
		{BackendName: "b2", Encrypted: true},
		encryptedSibling("b3", n),
	}
	decision, enc := ClassifyImport(envelopeHeader(n), siblings)
	if decision != ImportAdoptKey {
		t.Fatalf("decision = %v, want ImportAdoptKey from the one usable sibling", decision)
	}
	if enc.KeyID != "key-1" {
		t.Errorf("adopted the wrong sibling: %+v", enc)
	}
}

// TestImportDecision_String verifies each decision renders a distinct label,
// since the metric and audit trail are keyed on it.
func TestImportDecision_String(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, d := range []ImportDecision{ImportPlaintext, ImportAdoptKey, ImportUnreadable} {
		s := d.String()
		if s == "" || s == "unknown" || seen[s] {
			t.Errorf("decision %d rendered %q, want a distinct label", d, s)
		}
		seen[s] = true
	}
}
