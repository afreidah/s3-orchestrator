// -------------------------------------------------------------------------------
// Range Translation Tests
//
// Author: Alex Freidah
//
// Tests for plaintext-to-ciphertext range translation including aligned chunks,
// cross-boundary ranges, single-byte ranges, and invalid inputs.
// -------------------------------------------------------------------------------

package encryption

import (
	"testing"
)

func TestCiphertextRange_FirstChunk(t *testing.T) {
	t.Parallel()
	r, err := CiphertextRange(0, 63, 64)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}
	if r.StartChunk != 0 {
		t.Errorf("StartChunk = %d, want 0", r.StartChunk)
	}
	if r.SliceStart != 0 {
		t.Errorf("SliceStart = %d, want 0", r.SliceStart)
	}
	if r.SliceLen != 64 {
		t.Errorf("SliceLen = %d, want 64", r.SliceLen)
	}
	if r.BackendRange == "" {
		t.Error("BackendRange should not be empty")
	}
}

func TestCiphertextRange_CrossBoundary(t *testing.T) {
	t.Parallel()
	// bytes 60-70 crosses chunk 0 (0-63) and chunk 1 (64-127)
	r, err := CiphertextRange(60, 70, 64)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}
	if r.StartChunk != 0 {
		t.Errorf("StartChunk = %d, want 0", r.StartChunk)
	}
	if r.SliceStart != 60 {
		t.Errorf("SliceStart = %d, want 60", r.SliceStart)
	}
	if r.SliceLen != 11 {
		t.Errorf("SliceLen = %d, want 11", r.SliceLen)
	}
}

func TestCiphertextRange_SecondChunk(t *testing.T) {
	t.Parallel()
	// bytes 64-127 → chunk 1 only
	r, err := CiphertextRange(64, 127, 64)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}
	if r.StartChunk != 1 {
		t.Errorf("StartChunk = %d, want 1", r.StartChunk)
	}
	if r.SliceStart != 0 {
		t.Errorf("SliceStart = %d, want 0", r.SliceStart)
	}
	if r.SliceLen != 64 {
		t.Errorf("SliceLen = %d, want 64", r.SliceLen)
	}
}

func TestCiphertextRange_SingleByte(t *testing.T) {
	t.Parallel()
	r, err := CiphertextRange(100, 100, 64)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}
	if r.StartChunk != 1 {
		t.Errorf("StartChunk = %d, want 1", r.StartChunk)
	}
	if r.SliceStart != 36 { // 100 % 64 = 36
		t.Errorf("SliceStart = %d, want 36", r.SliceStart)
	}
	if r.SliceLen != 1 {
		t.Errorf("SliceLen = %d, want 1", r.SliceLen)
	}
}

func TestCiphertextRange_InvalidRange(t *testing.T) {
	t.Parallel()
	_, err := CiphertextRange(10, 5, 64)
	if err == nil {
		t.Error("expected error for end < start")
	}

	_, err = CiphertextRange(-1, 5, 64)
	if err == nil {
		t.Error("expected error for negative start")
	}
}
