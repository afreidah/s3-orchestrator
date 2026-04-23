// -------------------------------------------------------------------------------
// store.objectLocationFromDB unit tests
//
// Author: Alex Freidah
//
// objectLocationFromDB is a pure helper that flattens nullable database
// columns into an ObjectLocation. Exercised end-to-end via toObjectLocation
// in integration paths; these unit tests cover each nullable-field branch
// directly without requiring a PostgreSQL connection.
// -------------------------------------------------------------------------------

package store

import (
	"testing"
	"time"
)

// TestObjectLocationFromDB_AllFieldsPopulated covers the happy path where
// every nullable column carries a value.
func TestObjectLocationFromDB_AllFieldsPopulated(t *testing.T) {
	t.Parallel()
	keyID := "key-abc"
	var ptSize int64 = 4096
	hash := "sha256:deadbeef"
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	loc := objectLocationFromDB(&dbObjectRow{
		Key:           "obj/foo",
		Backend:       "b1",
		Size:          4200,
		Encrypted:     true,
		EncryptionKey: []byte{0xde, 0xad, 0xbe, 0xef},
		KeyID:         &keyID,
		PlaintextSize: &ptSize,
		ContentHash:   &hash,
		CreatedAt:     created,
	})

	if loc.ObjectKey != "obj/foo" || loc.BackendName != "b1" || loc.SizeBytes != 4200 {
		t.Errorf("basic fields: %+v", loc)
	}
	if !loc.Encrypted || string(loc.EncryptionKey) != "\xde\xad\xbe\xef" {
		t.Errorf("encryption fields: encrypted=%v key=%x", loc.Encrypted, loc.EncryptionKey)
	}
	if loc.KeyID != keyID || loc.PlaintextSize != ptSize || loc.ContentHash != hash {
		t.Errorf("nullable fields: keyID=%q ptSize=%d hash=%q", loc.KeyID, loc.PlaintextSize, loc.ContentHash)
	}
	if !loc.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", loc.CreatedAt, created)
	}
}

// TestObjectLocationFromDB_NilNullableFields covers the non-encrypted path
// where KeyID, PlaintextSize, and ContentHash are all nil — they must
// flatten to their zero values, not panic.
func TestObjectLocationFromDB_NilNullableFields(t *testing.T) {
	t.Parallel()
	loc := objectLocationFromDB(&dbObjectRow{
		Key:       "obj/bar",
		Backend:   "b2",
		Size:      1024,
		Encrypted: false,
		// KeyID / PlaintextSize / ContentHash intentionally nil
		CreatedAt: time.Now(),
	})

	if loc.KeyID != "" {
		t.Errorf("KeyID = %q, want empty", loc.KeyID)
	}
	if loc.PlaintextSize != 0 {
		t.Errorf("PlaintextSize = %d, want 0", loc.PlaintextSize)
	}
	if loc.ContentHash != "" {
		t.Errorf("ContentHash = %q, want empty", loc.ContentHash)
	}
}

// TestObjectLocationFromDB_PartialNilFields covers mixed-null rows (for
// example an encrypted object with no content hash yet).
func TestObjectLocationFromDB_PartialNilFields(t *testing.T) {
	t.Parallel()
	keyID := "key-x"
	loc := objectLocationFromDB(&dbObjectRow{
		Key:       "obj/baz",
		Backend:   "b3",
		Size:      512,
		Encrypted: true,
		KeyID:     &keyID,
		// PlaintextSize and ContentHash still nil
		CreatedAt: time.Now(),
	})
	if loc.KeyID != keyID {
		t.Errorf("KeyID = %q, want %q", loc.KeyID, keyID)
	}
	if loc.PlaintextSize != 0 || loc.ContentHash != "" {
		t.Errorf("expected other nullable fields zero; got ptSize=%d hash=%q", loc.PlaintextSize, loc.ContentHash)
	}
}