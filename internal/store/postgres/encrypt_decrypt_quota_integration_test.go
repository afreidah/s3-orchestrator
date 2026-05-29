// -----------------------------------------------------------------------------
// MarkObjectEncrypted / MarkObjectDecrypted Quota Integration Tests
//
// Author: Alex Freidah
//
// Pins the contract that the bulk encrypt-existing and decrypt-existing admin
// paths keep backend_quotas.bytes_used in step with object_locations.size_bytes.
// Each rewrite changes the on-disk byte count (encryption adds per-chunk
// overhead, decryption removes it), so the per-backend bytes_used counter
// must follow the size delta. Without these adjustments the counter drifts
// permanently from SUM(object_locations.size_bytes) and write-routing
// silently overcommits.
// -----------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"testing"
)

// TestStoreInt_MarkObjectEncrypted_AdjustsBytesUsed asserts that marking an
// object encrypted advances backend_quotas.bytes_used by ciphertextSize -
// plaintextSize so the counter reflects the new on-disk byte count.
func TestStoreInt_MarkObjectEncrypted_AdjustsBytesUsed(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	const (
		plaintextSize  = int64(100)
		ciphertextSize = int64(200) // per-chunk overhead inflates by 100
	)
	resetBytesUsed(t, s, "backend-a")
	key := uniqueKey(t, "encrypt-quota")

	if _, err := s.RecordObject(ctx, key, "backend-a", plaintextSize, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	before := readBytesUsed(t, s, "backend-a")

	if err := s.MarkObjectEncrypted(ctx, key, "backend-a", []byte("k"), "test-key", plaintextSize, ciphertextSize); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}

	after := readBytesUsed(t, s, "backend-a")
	if delta := after - before; delta != ciphertextSize-plaintextSize {
		t.Errorf("bytes_used delta = %d, want %d (ciphertext - plaintext)", delta, ciphertextSize-plaintextSize)
	}
}

// TestStoreInt_MarkObjectDecrypted_AdjustsBytesUsed asserts that marking an
// object decrypted retreats bytes_used by ciphertextSize - plaintextSize so
// the counter reflects the smaller on-disk byte count.
func TestStoreInt_MarkObjectDecrypted_AdjustsBytesUsed(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	const (
		plaintextSize  = int64(100)
		ciphertextSize = int64(200)
	)
	resetBytesUsed(t, s, "backend-a")
	key := uniqueKey(t, "decrypt-quota")

	if _, err := s.RecordObject(ctx, key, "backend-a", plaintextSize, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if err := s.MarkObjectEncrypted(ctx, key, "backend-a", []byte("k"), "test-key", plaintextSize, ciphertextSize); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}
	beforeDecrypt := readBytesUsed(t, s, "backend-a")

	if err := s.MarkObjectDecrypted(ctx, key, "backend-a", plaintextSize); err != nil {
		t.Fatalf("MarkObjectDecrypted: %v", err)
	}

	after := readBytesUsed(t, s, "backend-a")
	if delta := beforeDecrypt - after; delta != ciphertextSize-plaintextSize {
		t.Errorf("bytes_used delta = %d, want %d (ciphertext - plaintext freed)", delta, ciphertextSize-plaintextSize)
	}
}

// TestStoreInt_MarkObjectEncrypted_ZeroDeltaNoOp asserts that when ciphertext
// equals plaintext (the synthetic case where the encoder is a passthrough)
// the quota counter stays untouched.
func TestStoreInt_MarkObjectEncrypted_ZeroDeltaNoOp(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	const size = int64(100)
	resetBytesUsed(t, s, "backend-a")
	key := uniqueKey(t, "encrypt-zero")

	if _, err := s.RecordObject(ctx, key, "backend-a", size, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	before := readBytesUsed(t, s, "backend-a")

	if err := s.MarkObjectEncrypted(ctx, key, "backend-a", []byte("k"), "test-key", size, size); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}

	if got := readBytesUsed(t, s, "backend-a"); got != before {
		t.Errorf("bytes_used = %d, want %d (unchanged on zero-delta)", got, before)
	}
}

// TestStoreInt_MarkObjectEncrypted_BatchSumsCorrectly asserts the deltas are
// additive across a multi-object batch  -  the realistic case for bulk
// encrypt-existing where every object inflates.
func TestStoreInt_MarkObjectEncrypted_BatchSumsCorrectly(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()

	const (
		objects        = 5
		plaintextSize  = int64(100)
		ciphertextSize = int64(150)
		perObjectDelta = ciphertextSize - plaintextSize
	)
	resetBytesUsed(t, s, "backend-a")

	keys := make([]string, objects)
	for i := range objects {
		keys[i] = uniqueKey(t, "encrypt-batch")
		if _, err := s.RecordObject(ctx, keys[i], "backend-a", plaintextSize, nil); err != nil {
			t.Fatalf("RecordObject %d: %v", i, err)
		}
	}
	before := readBytesUsed(t, s, "backend-a")

	for i, k := range keys {
		if err := s.MarkObjectEncrypted(ctx, k, "backend-a", []byte("k"), "test-key", plaintextSize, ciphertextSize); err != nil {
			t.Fatalf("MarkObjectEncrypted %d: %v", i, err)
		}
	}

	after := readBytesUsed(t, s, "backend-a")
	want := before + int64(objects)*perObjectDelta
	if after != want {
		t.Errorf("bytes_used after batch = %d, want %d (before + %d objects * %d delta)", after, want, objects, perObjectDelta)
	}
}

// readBytesUsed returns the current bytes_used value for backendName.
func readBytesUsed(t *testing.T, s *Store, backendName string) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT bytes_used FROM backend_quotas WHERE backend_name = $1`, backendName,
	).Scan(&v); err != nil {
		t.Fatalf("read bytes_used: %v", err)
	}
	return v
}

// resetBytesUsed zeroes the bytes_used column on a backend so each test
// starts from a known baseline regardless of which other tests in the
// shared fixture have already run.
func resetBytesUsed(t *testing.T, s *Store, backendName string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE backend_quotas SET bytes_used = 0 WHERE backend_name = $1`, backendName,
	); err != nil {
		t.Fatalf("reset bytes_used: %v", err)
	}
}
