// -------------------------------------------------------------------------------
// Import Classification Tests
//
// Author: Alex Freidah
//
// Drives ClassifyImport against real ciphertext produced by the encryptor,
// covering the shape of the incident it exists to prevent: an already-
// encrypted object rediscovered on a backend whose ledger row is gone.
// -------------------------------------------------------------------------------

package reconcile

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// siblingStub returns a fixed copy set for every key.
type siblingStub struct {
	locs []core.ObjectLocation
	err  error
}

// GetAllObjectLocations returns the seeded copy set or error.
func (s siblingStub) GetAllObjectLocations(context.Context, string) ([]core.ObjectLocation, error) {
	return s.locs, s.err
}

// newTestEncryptor builds an encryptor over a fixed master key.
func newTestEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	p, err := encryption.NewConfigKeyProvider(key, "key-1")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(p, 4096)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

// encryptInto encrypts body onto be under key and returns the row metadata the
// write path would have recorded for it.
func encryptInto(t *testing.T, enc *encryption.Encryptor, be *backendtest.InMemory, key, body string) core.ObjectLocation {
	t.Helper()
	res, err := enc.Encrypt(context.Background(), strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	be.Objects[key] = backendtest.Object{Data: ct}
	return core.ObjectLocation{
		BackendName:   "b1",
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
		KeyID:         res.KeyID,
		PlaintextSize: int64(len(body)),
	}
}

// classify runs ClassifyImport against a backend and copy set, with no codec:
// these cases are about the encryption envelope, which is read from the head of
// the object and needs nothing else.
func classify(t *testing.T, be *backendtest.InMemory, key string, locs []core.ObjectLocation) (*core.StoredForm, error) {
	t.Helper()
	// A key with nothing behind it is size 0, which is what the caller would
	// have observed listing a backend that lost the object mid-pass.
	var size int64
	if obj, ok := be.Get(key); ok {
		size = int64(len(obj.Data))
	}
	return ClassifyImport(context.Background(), ClassifyDeps{
		Backend: be,
		Stores:  siblingStub{locs: locs},
		Source:  "test",
	}, "b1", key, size)
}

// TestClassifyImport_AdoptsKeyFromSurvivingReplica is the recovery the
// incident had to be done by hand: a replica row survived with the key, so the
// rediscovered copy inherits it and stays readable.
func TestClassifyImport_AdoptsKeyFromSurvivingReplica(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	be := backendtest.NewInMemory()
	sib := encryptInto(t, enc, be, "traces/block", "block meta payload")

	got, err := classify(t, be, "traces/block", []core.ObjectLocation{sib})
	if err != nil {
		t.Fatalf("ClassifyImport: %v", err)
	}
	if got == nil || !got.Encrypted {
		t.Fatalf("want encrypted metadata, got %+v", got)
	}
	if got.KeyID != sib.KeyID || !bytes.Equal(got.EncryptionKey, sib.EncryptionKey) {
		t.Errorf("key not adopted from the surviving replica: %+v", got)
	}

	// The adopted key must actually decrypt the bytes it was adopted for.
	body := be.Objects["traces/block"].Data
	plain, _, err := enc.DecryptStored(context.Background(), bytes.NewReader(body),
		got.EncryptionKey, got.KeyID, got.PlaintextSize, nil)
	if err != nil {
		t.Fatalf("adopted key failed to decrypt: %v", err)
	}
	out, err := io.ReadAll(plain)
	if err != nil {
		t.Fatalf("read plaintext: %v", err)
	}
	if string(out) != "block meta payload" {
		t.Errorf("decrypted %q, want the original payload", out)
	}
}

// TestClassifyImport_RefusesKeyFromLaterWrite covers the trap: a row for the
// same key exists, but it describes a later write with its own DEK. The stray
// bytes are an earlier ciphertext that key cannot open, so adopting it would
// record an object that claims to be readable and is not.
func TestClassifyImport_RefusesKeyFromLaterWrite(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	be := backendtest.NewInMemory()

	// The stray copy on the backend is the earlier write.
	encryptInto(t, enc, be, "traces/block", "version one")
	// The ledger's surviving row describes a second, independent write.
	later := encryptInto(t, enc, backendtest.NewInMemory(), "traces/block", "version two")

	got, err := classify(t, be, "traces/block", []core.ObjectLocation{later})
	if err != nil {
		t.Fatalf("ClassifyImport: %v", err)
	}
	if got == nil || !got.Encrypted {
		t.Fatalf("an envelope must never be recorded as plaintext, got %+v", got)
	}
	if len(got.EncryptionKey) != 0 {
		t.Error("adopted a key from an unrelated write")
	}
}

// TestClassifyImport_PlaintextObject verifies an unencrypted discovery is
// imported with no metadata and without paying for a ledger lookup.
func TestClassifyImport_PlaintextObject(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Objects["logs/app.txt"] = backendtest.Object{Data: []byte("plain log line")}

	got, err := classify(t, be, "logs/app.txt", nil)
	if err != nil {
		t.Fatalf("ClassifyImport: %v", err)
	}
	if got != nil {
		t.Errorf("plaintext must carry no encryption metadata, got %+v", got)
	}
}

// TestClassifyImport_ShortObject verifies an object too small to hold a header
// is treated as plaintext rather than erroring.
func TestClassifyImport_ShortObject(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Objects["tiny"] = backendtest.Object{Data: []byte("hi")}

	got, err := classify(t, be, "tiny", nil)
	if err != nil {
		t.Fatalf("ClassifyImport: %v", err)
	}
	if got != nil {
		t.Errorf("a short object is plaintext, got %+v", got)
	}
}

// TestClassifyImport_BackendReadFails verifies an unreadable object aborts the
// import rather than defaulting to plaintext, which is the failure mode that
// would reintroduce the bug.
func TestClassifyImport_BackendReadFails(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	if _, err := classify(t, be, "missing", nil); err == nil {
		t.Error("expected an error when the object cannot be read")
	}
}
