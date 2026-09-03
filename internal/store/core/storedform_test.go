// -------------------------------------------------------------------------------
// Stored-Form Rewrite Tests
//
// Author: Alex Freidah
//
// What each rewrite has to move the backend's byte counter by, and the cases
// where it must not touch it at all. The arithmetic is the whole reason these
// operations live in one place, so it is asserted here rather than per engine.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"testing"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// compressedTo builds the update a recompression pass would record for a copy
// that now occupies size bytes.
func compressedTo(size int64) *CompressedUpdate {
	return &CompressedUpdate{
		ObjectKey:   "k",
		BackendName: "b1",
		Algorithm:   "zstd",
		Level:       "default",
		SizeBytes:   size,
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestMarkObjectCompressed_ChargesTheSizeDifference pins the contract every
// stored-form rewrite shares: the row and the counter move together, by
// exactly what the copy's size changed by.
func TestMarkObjectCompressed_ChargesTheSizeDifference(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{}
	runner := &stubRunner{tx: stub}

	if err := MarkObjectCompressed(context.Background(), runner, compressedTo(400), 1000); err != nil {
		t.Fatalf("MarkObjectCompressed: %v", err)
	}

	if len(stub.compressed) != 1 || stub.compressed[0].SizeBytes != 400 {
		t.Fatalf("stored form = %#v, want the rewritten copy recorded once", stub.compressed)
	}
	if len(stub.adjustments) != 1 {
		t.Fatalf("adjustments = %#v, want exactly one", stub.adjustments)
	}
	if got := stub.adjustments[0]; got.backend != "b1" || got.delta != -600 {
		t.Errorf("adjustment = %+v, want b1 credited the 600 bytes compression saved", got)
	}
}

// TestMarkObjectCompressed_UnchangedSizeLeavesTheCounterAlone covers the case
// a rewrite reaches often: re-encoding that lands on the same size must not
// write the counter back, because a no-op update still costs a row version.
func TestMarkObjectCompressed_UnchangedSizeLeavesTheCounterAlone(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{}
	runner := &stubRunner{tx: stub}

	if err := MarkObjectCompressed(context.Background(), runner, compressedTo(1000), 1000); err != nil {
		t.Fatalf("MarkObjectCompressed: %v", err)
	}

	if len(stub.compressed) != 1 {
		t.Fatalf("stored form = %#v, want the row still written", stub.compressed)
	}
	if len(stub.adjustments) != 0 {
		t.Errorf("adjustments = %#v, want none for a rewrite that changed no bytes", stub.adjustments)
	}
}

// TestMarkObjectCompressed_RowFailureSkipsTheQuota pins that the counter is
// never moved for a row that was not written: the two halves are one
// transaction, and the quota half is reached only after the row half.
func TestMarkObjectCompressed_RowFailureSkipsTheQuota(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{formErr: errors.New("boom")}
	runner := &stubRunner{tx: stub}

	err := MarkObjectCompressed(context.Background(), runner, compressedTo(400), 1000)
	if err == nil {
		t.Fatal("expected the row failure to surface")
	}
	if len(stub.adjustments) != 0 {
		t.Errorf("adjustments = %#v, want none when the row was not written", stub.adjustments)
	}
}

// TestMarkObjectEncrypted_ChargesTheEnvelopeOverhead asserts the direction
// that grows: the ciphertext is larger than the plaintext it replaced, and the
// backend is charged the difference.
func TestMarkObjectEncrypted_ChargesTheEnvelopeOverhead(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{}
	runner := &stubRunner{tx: stub}

	if err := MarkObjectEncrypted(context.Background(), runner, &EncryptedUpdate{
		ObjectKey: "k", BackendName: "b1", EncryptionKey: []byte("dek"), KeyID: "kid",
		PlaintextSize: 1000, CiphertextSize: 1064,
	}); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}

	if len(stub.encrypted) != 1 || stub.encrypted[0].sizeBytes != 1064 {
		t.Fatalf("encrypted = %#v, want the copy recorded at its ciphertext size", stub.encrypted)
	}
	if len(stub.adjustments) != 1 || stub.adjustments[0].delta != 64 {
		t.Errorf("adjustments = %#v, want the 64 bytes the envelope added", stub.adjustments)
	}
}

// TestMarkObjectDecrypted_CreditsWhatTheEnvelopeCost covers the read this
// operation cannot do without: the delta comes from the size stored before the
// row is overwritten, not from anything the caller knows.
func TestMarkObjectDecrypted_CreditsWhatTheEnvelopeCost(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{copySize: 1064}
	runner := &stubRunner{tx: stub}

	if err := MarkObjectDecrypted(context.Background(), runner, "k", "b1", 1000); err != nil {
		t.Fatalf("MarkObjectDecrypted: %v", err)
	}

	if len(stub.decrypted) != 1 || stub.decrypted[0].sizeBytes != 1000 {
		t.Fatalf("decrypted = %#v, want the copy recorded at its plaintext size", stub.decrypted)
	}
	if len(stub.adjustments) != 1 || stub.adjustments[0].delta != -64 {
		t.Errorf("adjustments = %#v, want the 64 bytes the envelope cost credited back", stub.adjustments)
	}
}

// TestMarkObjectDecrypted_SizeReadFailureWritesNothing pins that a copy whose
// current size cannot be read is left alone: without that size the delta would
// be computed against zero and the counter would be wrong in both places.
func TestMarkObjectDecrypted_SizeReadFailureWritesNothing(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{copySizeErr: errors.New("boom")}
	runner := &stubRunner{tx: stub}

	if err := MarkObjectDecrypted(context.Background(), runner, "k", "b1", 1000); err == nil {
		t.Fatal("expected the size read failure to surface")
	}
	if len(stub.decrypted) != 0 || len(stub.adjustments) != 0 {
		t.Errorf("decrypted=%#v adjustments=%#v, want neither written", stub.decrypted, stub.adjustments)
	}
}

// TestMarkObjectEncrypted_QuotaFailureSurfaces asserts the quota half is not
// best-effort: a counter that cannot be moved fails the whole rewrite, so the
// transaction rolls back rather than leaving the row and the counter disagreeing.
func TestMarkObjectEncrypted_QuotaFailureSurfaces(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{adjustErr: errors.New("boom")}
	runner := &stubRunner{tx: stub}

	if err := MarkObjectEncrypted(context.Background(), runner, &EncryptedUpdate{
		ObjectKey: "k", BackendName: "b1", EncryptionKey: []byte("dek"), KeyID: "kid",
		PlaintextSize: 1000, CiphertextSize: 1064,
	}); err == nil {
		t.Fatal("expected the quota failure to surface")
	}
}
