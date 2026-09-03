// -------------------------------------------------------------------------------
// Read-Path Envelope Verification Tests
//
// Author: Alex Freidah
//
// Coverage for verifyStoredEnvelope: the read path must refuse a copy whose
// stored bytes disagree with its row, must leave the body fully readable when
// they agree, and must skip the check for ranges that cannot carry the
// signature in the first place.
// -------------------------------------------------------------------------------

package object

import (
	"errors"
	"io"
	"strings"
	"testing"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// envelopeResult builds a GetObjectResult whose body carries the given bytes.
func envelopeResult(body string) *s3be.GetObjectResult {
	return &s3be.GetObjectResult{Body: io.NopCloser(strings.NewReader(body)), Size: int64(len(body))}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestVerifyStoredEnvelope_Agreement verifies a copy whose bytes match its row
// passes and still streams its full body afterwards.
func TestVerifyStoredEnvelope_Agreement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		body      string
		encrypted bool
	}{
		{"envelope on an encrypted row", "SENC\x01ciphertext", true},
		{"plaintext on a plain row", "plain object body", false},
		{"tiny plaintext on a plain row", "ab", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := envelopeResult(tt.body)
			if err := verifyStoredEnvelope(r, &core.ObjectLocation{Encrypted: tt.encrypted}, ""); err != nil {
				t.Fatalf("verifyStoredEnvelope: %v", err)
			}
			got, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(got) != tt.body {
				t.Errorf("body = %q, want the original %q", got, tt.body)
			}
		})
	}
}

// TestVerifyStoredEnvelope_Divergence verifies both directions of disagreement
// are rejected rather than served: ciphertext handed out as plaintext, and
// plaintext about to be run through the decryptor.
func TestVerifyStoredEnvelope_Divergence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		body      string
		encrypted bool
	}{
		{"envelope on a row that claims plaintext", "SENC\x01ciphertext", false},
		{"plaintext on a row that claims encryption", "not an envelope", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := verifyStoredEnvelope(envelopeResult(tt.body), &core.ObjectLocation{Encrypted: tt.encrypted}, "")
			if !errors.Is(err, core.ErrEncryptionFlagMismatch) {
				t.Errorf("expected ErrEncryptionFlagMismatch, got %v", err)
			}
		})
	}
}

// TestVerifyStoredEnvelope_SkipsUninspectableReads verifies the check is only
// applied where the signature would actually be: a nil row has nothing to
// contradict, and a range starting past byte 0 carries no signature, so
// neither may be reported as a mismatch.
func TestVerifyStoredEnvelope_SkipsUninspectableReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		loc  *core.ObjectLocation
		rng  string
		body string
	}{
		{"no location row", nil, "", "SENC\x01ciphertext"},
		{"ciphertext range starts past the header", &core.ObjectLocation{Encrypted: true}, "bytes=32-95", "chunk bytes"},
		{"mid-object range on a plain row", &core.ObjectLocation{}, "bytes=100-199", "SENC\x01looks like magic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := verifyStoredEnvelope(envelopeResult(tt.body), tt.loc, tt.rng); err != nil {
				t.Errorf("uninspectable read must not fail, got %v", err)
			}
		})
	}
}

// TestVerifyStoredEnvelope_ChecksRangeFromZero verifies the one ranged case
// that can be inspected is: a plain row read from byte 0 that turns out to
// hold an envelope is the silent ciphertext-as-plaintext leak, so it must fail.
func TestVerifyStoredEnvelope_ChecksRangeFromZero(t *testing.T) {
	t.Parallel()
	err := verifyStoredEnvelope(envelopeResult("SENC\x01ciphertext"), &core.ObjectLocation{}, "bytes=0-99")
	if !errors.Is(err, core.ErrEncryptionFlagMismatch) {
		t.Errorf("expected ErrEncryptionFlagMismatch, got %v", err)
	}
}
