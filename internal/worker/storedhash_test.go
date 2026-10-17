// -------------------------------------------------------------------------------
// Stored Hash Tests
//
// Author: Alex Freidah
//
// The hasher opens a copy through its recorded layers before hashing it. A copy
// it has no key or codec for, or whose bytes contradict the row's encryption
// flag, has to come back as an error the scrubber treats as unreadable rather
// than as a digest of the wrong bytes.
// -------------------------------------------------------------------------------

package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// hashBody is the plaintext the envelope fixtures seal.
const hashBody = "bytes the client wrote"

// sealedForHash encrypts hashBody with a throwaway key and returns the stored
// envelope with the row that records it.
func sealedForHash(t *testing.T) ([]byte, core.ObjectLocation) {
	t.Helper()
	provider, err := encryption.NewConfigKeyProvider(base64.StdEncoding.EncodeToString(make([]byte, 32)), "key-1")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 4096)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	res, err := enc.Encrypt(context.Background(), bytes.NewReader([]byte(hashBody)), int64(len(hashBody)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	return ciphertext, core.ObjectLocation{
		ObjectKey:   "bucket/key1",
		BackendName: "b1",
		SizeBytes:   int64(len(ciphertext)),
		StoredForm: core.StoredForm{
			Encrypted:     true,
			EncryptionKey: res.WrappedDEK,
			KeyID:         res.KeyID,
			PlaintextSize: int64(len(hashBody)),
			ContentHash:   hashString(hashBody),
		},
	}
}

// serveStored has ops serve stored as the bytes of the b1 copy.
func serveStored(t *testing.T, ops *MockOps, stored []byte) {
	t.Helper()
	be := backendtest.NewMockObjectBackend(gomock.NewController(t))
	ops.EXPECT().GetBackend("b1").Return(be, nil).AnyTimes()
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetWithTimeout(gomock.Any(), be, gomock.Any(), "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(stored)),
		Size: int64(len(stored)),
	}, func() {}, nil).AnyTimes()
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestHashStored_EncryptedWithoutEncryptor verifies an envelope the hasher has
// no encryptor for is refused with ErrNoEncryptor rather than hashed as if
// the ciphertext were the client's bytes.
func TestHashStored_EncryptedWithoutEncryptor(t *testing.T) {
	t.Parallel()
	ops := newMockOps(gomock.NewController(t))
	stored, loc := sealedForHash(t)
	serveStored(t, ops, stored)

	_, err := newStoredHasher(ops, nil, nil, "storedhash-test").hashStored(context.Background(), &loc)
	if !errors.Is(err, storedform.ErrNoEncryptor) {
		t.Fatalf("err = %v, want ErrNoEncryptor", err)
	}
}

// TestHashStored_FlagMismatchCountsTheSource verifies bytes that contradict the
// row's encryption flag surface as ErrEncryptionFlagMismatch and increment the
// mismatch counter under the hasher's source label, in both directions.
func TestHashStored_FlagMismatchCountsTheSource(t *testing.T) {
	t.Parallel()
	envelope, sealedRow := sealedForHash(t)
	plainRow := core.ObjectLocation{ObjectKey: "bucket/key1", BackendName: "b1", SizeBytes: int64(len(envelope))}

	cases := []struct {
		name   string
		stored []byte
		loc    core.ObjectLocation
	}{
		{"row plain, bytes sealed", envelope, plainRow},
		{"row sealed, bytes plain", bytes.Repeat([]byte("p"), len(envelope)), sealedRow},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ops := newMockOps(gomock.NewController(t))
			serveStored(t, ops, tc.stored)
			source := fmt.Sprintf("storedhash-mismatch-%d", i)
			counter := telemetry.EncryptionFlagMismatchTotal.WithLabelValues(source)
			before := promtest.ToFloat64(counter)

			_, err := newStoredHasher(ops, nil, nil, source).hashStored(context.Background(), &tc.loc)
			if !errors.Is(err, core.ErrEncryptionFlagMismatch) {
				t.Fatalf("err = %v, want ErrEncryptionFlagMismatch", err)
			}
			if got := promtest.ToFloat64(counter) - before; got != 1 {
				t.Errorf("mismatch counter moved by %v, want 1", got)
			}
		})
	}
}

// TestUndecodable classifies the hashing errors no later pass could clear
// apart from an outage that might.
func TestUndecodable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("open: %w", core.ErrEncryptionFlagMismatch), true},
		{fmt.Errorf("open: %w", storedform.ErrNoCodec), true},
		{fmt.Errorf("open: %w", storedform.ErrNoEncryptor), true},
		{errors.New("connection reset"), false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := undecodable(tc.err); got != tc.want {
			t.Errorf("undecodable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestHashCopies_EncryptedWithoutEncryptorIsUnreadable verifies the hash pass
// skips an envelope it holds no encryptor for as unreadable, writing no hash,
// rather than failing it or recording a digest of the ciphertext.
func TestHashCopies_EncryptedWithoutEncryptorIsUnreadable(t *testing.T) {
	t.Parallel()
	s, ops, _, _, ms := setupScrubber(t)
	stored, loc := sealedForHash(t)
	loc.ContentHash = ""
	serveStored(t, ops, stored)

	res := s.hashOne(context.Background(), &loc)
	if res.Outcome != batch.ItemSkipped || res.Status != progress.StatusUnreadable {
		t.Errorf("result = %+v, want skipped as unreadable", res)
	}
	if ms.lastUpdatedHash != "" {
		t.Errorf("stored hash %q, want none", ms.lastUpdatedHash)
	}
}
