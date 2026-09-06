// -------------------------------------------------------------------------------
// PutObject Encryption - One Ciphertext Per Write
//
// Author: Alex Freidah
//
// Covers the property the write path's encrypt pass exists to hold: a write
// encrypts once and every upload it makes replays those bytes, so the copies of
// a key never diverge. Encryption mechanics themselves are covered in
// encryption_helpers_test.go; these drive PutObject end to end.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// recordingRejector reads the body of a PutObject it is about to fail. The
// shared in-memory backend drops the body when PutErr is set, which leaves a
// failed attempt with nothing to compare against; a test that asks whether two
// uploads sent the same bytes needs the bytes of the one that failed.
type recordingRejector struct {
	*backendtest.InMemory
	mu   sync.Mutex
	sent [][]byte
	err  error
}

// PutObject records the body and fails.
func (r *recordingRejector) PutObject(_ context.Context, _ string, body io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	r.sent = append(r.sent, data)
	r.mu.Unlock()
	return "", r.err
}

// lastSent returns the body of the most recent attempt this backend rejected.
func (r *recordingRejector) lastSent(t *testing.T) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sent) == 0 {
		t.Fatal("backend recorded no upload")
	}
	return r.sent[len(r.sent)-1]
}

// newRecordingRejector builds a backend that captures each upload and fails it.
func newRecordingRejector() *recordingRejector {
	return &recordingRejector{InMemory: backendtest.NewInMemory(), err: errors.New("backend rejected the write")}
}

// -------------------------------------------------------------------------
// FAILOVER
// -------------------------------------------------------------------------

// TestPutObject_EncryptsOnceAcrossFailover asserts every attempt of one write
// sends byte-identical ciphertext under a single wrapped DEK. Encrypting per
// attempt would put a fresh base nonce on each upload, which is invisible while
// one copy lands but leaves the copies of a key differing once a write places
// several: replication moves bytes verbatim and describes the target row from
// the source, so nothing downstream would ever compare them.
//
// Three backends rather than two, since nothing here should hold for a reason
// peculiar to a pair.
func TestPutObject_EncryptsOnceAcrossFailover(t *testing.T) {
	t.Parallel()
	b1, b2 := newRecordingRejector(), newRecordingRejector()
	b3 := backendtest.NewInMemory()

	enc, cp := newCountingEncryptor(t)
	store, _ := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2, "b3": b3}, &fleetOpts{
		Order:     []string{"b1", "b2", "b3"},
		Encryptor: enc,
	})

	payload := []byte("one ciphertext, however many backends it takes")
	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: "enc-key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	stored, ok := b3.Get("enc-key")
	if !ok {
		t.Fatal("object should be on the backend that accepted it")
	}
	if bytes.Equal(stored.Data, payload) {
		t.Fatal("stored bytes are the plaintext; the write did not encrypt")
	}
	if got := b1.lastSent(t); !bytes.Equal(got, stored.Data) {
		t.Errorf("first attempt sent %d bytes, the stored copy is %d, and they differ", len(got), len(stored.Data))
	}
	if got := b2.lastSent(t); !bytes.Equal(got, stored.Data) {
		t.Errorf("second attempt sent %d bytes, the stored copy is %d, and they differ", len(got), len(stored.Data))
	}
	if got := cp.wraps.Load(); got != 1 {
		t.Errorf("WrapDEK called %d times across three attempts, want 1", got)
	}
}

// TestPutObject_EncryptFailureRejectsTheWrite asserts a KeyProvider that cannot
// wrap a DEK fails the write before any backend is touched. The encrypt pass
// runs ahead of the failover loop, so there is no attempt left that could put
// the plaintext somewhere.
func TestPutObject_EncryptFailureRejectsTheWrite(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()

	enc, cp := newCountingEncryptor(t)
	cp.wrapErr = errors.New("key provider unreachable")
	store, c := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, &fleetOpts{
		Order:     []string{"b1"},
		Encryptor: enc,
	})

	payload := []byte("must not land in the clear")
	_, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: "enc-key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain",
	})
	if err == nil {
		t.Fatal("expected the write to fail when the DEK cannot be wrapped")
	}
	if !errors.Is(err, cp.wrapErr) {
		t.Errorf("error chain does not contain the provider error: %v", err)
	}
	if be.Has("enc-key") {
		t.Error("nothing should have reached the backend")
	}
	if len(c.recordObject) != 0 {
		t.Errorf("recorded %d rows for a write that never uploaded", len(c.recordObject))
	}
}

// TestPutObject_RecordedEnvelopeDescribesStoredBytes asserts the row's key data
// decrypts what actually landed. The base nonce a row carries comes from the
// encrypt pass, and the bytes come from the body it materialized; a write that
// encrypted again after recording would leave the two describing different
// ciphertexts and the object unreadable.
func TestPutObject_RecordedEnvelopeDescribesStoredBytes(t *testing.T) {
	t.Parallel()
	b1 := newRecordingRejector()
	b2 := backendtest.NewInMemory()

	enc, _ := newCountingEncryptor(t)
	store, c := eligibleStore(t)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": b1, "b2": b2}, &fleetOpts{
		Order:     []string{"b1", "b2"},
		Encryptor: enc,
	})

	payload := []byte("the envelope on the row has to open the bytes on the backend")
	if _, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: "enc-key", Body: bytes.NewReader(payload), Size: int64(len(payload)), ContentType: "text/plain",
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if len(c.recordObject) != 1 {
		t.Fatalf("expected 1 RecordObject call, got %d", len(c.recordObject))
	}
	form := c.recordObject[0].Form
	if form == nil || !form.Encrypted {
		t.Fatalf("recorded form does not describe an encrypted object: %+v", form)
	}
	_, wrappedDEK, err := encryption.UnpackKeyData(form.EncryptionKey)
	if err != nil {
		t.Fatalf("unpack key data: %v", err)
	}

	stored, ok := b2.Get("enc-key")
	if !ok {
		t.Fatal("object should be on the backend that accepted it")
	}
	plainReader, err := enc.Decrypt(context.Background(), bytes.NewReader(stored.Data), wrappedDEK, form.KeyID)
	if err != nil {
		t.Fatalf("decrypt stored bytes with the recorded key data: %v", err)
	}
	got, err := io.ReadAll(plainReader)
	if err != nil {
		t.Fatalf("read plaintext: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("decrypted %q, want %q", got, payload)
	}
	if form.PlaintextSize != int64(len(payload)) {
		t.Errorf("form.PlaintextSize = %d, want %d", form.PlaintextSize, len(payload))
	}
}
