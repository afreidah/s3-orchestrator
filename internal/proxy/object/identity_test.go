// -------------------------------------------------------------------------------
// Object Manager - Stored Identity Tests
//
// Author: Alex Freidah
//
// Covers what the recorded ETag is for: a HEAD answered without touching a
// backend, a validator that does not change when a read fails over to another
// copy, and the write-back that gives a pre-identity object one on the first
// read that had to ask.
//
// The digest itself is asserted against the MD5 of the bytes the client sent,
// which is the contract a client checks its own upload against and the only
// reason the value is computed here rather than read off a backend.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: the ETag algorithm under test
	"encoding/hex"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"go.uber.org/mock/gomock"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// md5ETag renders the quoted MD5 of body, which is what S3 reports for a
// single-part upload of those bytes.
func md5ETag(body string) string {
	sum := md5.Sum([]byte(body)) //nolint:gosec // G401: see above
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestPutObject_ReturnsMD5OfClientBytes pins the write half of the contract:
// the ETag a client is handed is the digest of what it sent, not whatever the
// backend replied with.
func TestPutObject_ReturnsMD5OfClientBytes(t *testing.T) {
	t.Parallel()
	const body = "identity contract"
	be := backendtest.NewInMemory()
	store, _ := putObjectStore(t, "b1")
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	got, err := mgr.PutObject(context.Background(), &PutObjectRequest{
		Key: "key", Body: bytes.NewReader([]byte(body)), Size: int64(len(body)), ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if want := md5ETag(body); got != want {
		t.Errorf("etag = %q, want %q", got, want)
	}
}

// TestHeadObject_ServedFromMetadata_MakesNoBackendCall is the saving this
// column set exists for. The fleet holds a backend that fails every call, so a
// HEAD that answers at all proves it never asked one.
func TestHeadObject_ServedFromMetadata_MakesNoBackendCall(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.HeadErr = context.DeadlineExceeded

	store := locationsStore(t, []core.ObjectLocation{{
		ObjectKey: "key", BackendName: "b1", SizeBytes: 9, CreatedAt: lmCreatedAt,
		Identity: &core.ObjectIdentity{
			ETag:         `"abc123"`,
			ContentType:  "text/plain",
			UserMetadata: map[string]string{"colour": "green"},
		},
	}}, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	res, err := mgr.HeadObject(context.Background(), "key")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if res.ETag != `"abc123"` {
		t.Errorf("etag = %q, want the stored one", res.ETag)
	}
	if res.ContentType != "text/plain" {
		t.Errorf("content type = %q, want the stored one", res.ContentType)
	}
	if res.Metadata["colour"] != "green" {
		t.Errorf("metadata = %v, want the stored user metadata", res.Metadata)
	}
	if res.Size != 9 {
		t.Errorf("size = %d, want 9", res.Size)
	}
}

// TestHeadObject_NoStoredIdentity_RecordsWhatTheBackendReported covers the
// self-healing path every object written before this existed takes: the read
// falls through to the backend once, and what it learns is written to every
// copy of the key so the next one is answered locally.
func TestHeadObject_NoStoredIdentity_RecordsWhatTheBackendReported(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Put("key", &backendtest.Object{
		Data: []byte("headme"), ContentType: "application/json", ETag: `"backend-etag"`,
	})

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{ObjectKey: "key", BackendName: "b1", SizeBytes: 6, CreatedAt: lmCreatedAt}}, nil).
		AnyTimes()

	var recorded *core.ObjectIdentity
	store.EXPECT().RecordObjectIdentity(gomock.Any(), "key", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, id *core.ObjectIdentity) error {
			recorded = id
			return nil
		}).Times(1)
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)
	if _, err := mgr.HeadObject(context.Background(), "key"); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	if recorded == nil {
		t.Fatal("expected the backend's answer to be recorded")
	}
	if recorded.ETag != `"backend-etag"` {
		t.Errorf("etag = %q, want the backend value adopted for a verbatim copy", recorded.ETag)
	}
	if recorded.ContentType != "application/json" {
		t.Errorf("content type = %q, want the backend value", recorded.ContentType)
	}
}

// TestHeadObject_EncryptedCopy_DoesNotAdoptBackendETag is the other half of
// that rule. The stored bytes are an envelope, so the backend's ETag is a
// digest of ciphertext and must not become the object's: only the scrubber,
// which reads the plaintext back, can supply one.
func TestHeadObject_EncryptedCopy_DoesNotAdoptBackendETag(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Put("key", &backendtest.Object{Data: []byte("ciphertext"), ETag: `"ciphertext-etag"`})

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{
			ObjectKey: "key", BackendName: "b1", SizeBytes: 10, CreatedAt: lmCreatedAt,
			Encrypted: true, EncryptionKey: []byte("packed"), KeyID: "kid", PlaintextSize: 6,
		}}, nil).AnyTimes()

	var recorded *core.ObjectIdentity
	store.EXPECT().RecordObjectIdentity(gomock.Any(), "key", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, id *core.ObjectIdentity) error {
			recorded = id
			return nil
		}).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)
	if _, err := mgr.HeadObject(context.Background(), "key"); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	if recorded != nil && recorded.ETag != "" {
		t.Errorf("etag = %q, want none adopted from a copy stored as ciphertext", recorded.ETag)
	}
}

// TestHeadObject_EncryptedCopy_StopsRewritingOnceRecorded pins the end of the
// healing loop. A HEAD can never fill an encrypted object's ETag - that one is
// the scrubber's to supply - so once the columns it can fill are set, the write
// has nothing left to do and must stop running on every request.
func TestHeadObject_EncryptedCopy_StopsRewritingOnceRecorded(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	be.Put("key", &backendtest.Object{Data: []byte("ciphertext"), ETag: `"ciphertext-etag"`})

	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	objectsStubs(store)
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{{
			ObjectKey: "key", BackendName: "b1", SizeBytes: 10, CreatedAt: lmCreatedAt,
			Encrypted: true, EncryptionKey: []byte("packed"), KeyID: "kid", PlaintextSize: 6,
			Identity: &core.ObjectIdentity{ContentType: "application/json", UserMetadata: map[string]string{}},
		}}, nil).AnyTimes()

	writes := 0
	store.EXPECT().RecordObjectIdentity(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, string, *core.ObjectIdentity) error {
			writes++
			return nil
		}).AnyTimes()
	storetest.Permissive(store)

	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)
	if _, err := mgr.HeadObject(context.Background(), "key"); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}

	if writes != 0 {
		t.Errorf("identity writes = %d, want none: every column this HEAD could fill is already set", writes)
	}
}

// TestCopyIdentity_CarriesTheSourceETag pins that a copy is the same object:
// the destination reports the source's validator rather than deriving a new
// one from the backend it lands on.
func TestCopyIdentity_CarriesTheSourceETag(t *testing.T) {
	t.Parallel()
	locs := []core.ObjectLocation{{
		ObjectKey: "src", BackendName: "b1",
		Identity: &core.ObjectIdentity{ETag: `"source-etag"`, ContentType: "text/plain"},
	}}

	got := copyIdentity(locs, "application/json", map[string]string{"k": "v"})
	if got == nil {
		t.Fatal("identity = nil, want the source's")
	}
	if got.ETag != `"source-etag"` {
		t.Errorf("etag = %q, want the source's", got.ETag)
	}
	// The content type is the one the copy is written with, not the source
	// row's: a REPLACE-directive copy can change it.
	if got.ContentType != "application/json" {
		t.Errorf("content type = %q, want the one the copy is written with", got.ContentType)
	}
	if got.UserMetadata["k"] != "v" {
		t.Errorf("metadata = %v, want the copy's", got.UserMetadata)
	}
}

// TestCopyIdentity_SourceWithoutETagGivesNone covers copying an object that
// has not learned its identity yet: there is nothing worth recording, and the
// destination learns its own on the first read that has to ask.
func TestCopyIdentity_SourceWithoutETagGivesNone(t *testing.T) {
	t.Parallel()
	locs := []core.ObjectLocation{{ObjectKey: "src", BackendName: "b1"}}

	if got := copyIdentity(locs, "text/plain", nil); got != nil {
		t.Errorf("identity = %+v, want nil for a source with no ETag", got)
	}
	if got := copyIdentity(nil, "text/plain", nil); got != nil {
		t.Errorf("identity = %+v, want nil with no source rows", got)
	}
}

// TestCopyIdentity_NilMetadataBecomesEmpty pins the normalisation the write
// path relies on: a stored identity means "this object has no user metadata"
// rather than "nobody looked".
func TestCopyIdentity_NilMetadataBecomesEmpty(t *testing.T) {
	t.Parallel()
	locs := []core.ObjectLocation{{
		ObjectKey: "src", BackendName: "b1",
		Identity: &core.ObjectIdentity{ETag: `"e"`},
	}}

	got := copyIdentity(locs, "text/plain", nil)
	if got == nil || got.UserMetadata == nil {
		t.Fatalf("identity = %+v, want a non-nil empty metadata map", got)
	}
	if len(got.UserMetadata) != 0 {
		t.Errorf("metadata = %v, want empty", got.UserMetadata)
	}
}

// TestGetObject_ReportsStoredETagAcrossCopies is the failover bug this fixes.
// The two copies report different ETags of their own, and the read has to
// report the object's regardless of which one answers.
func TestGetObject_ReportsStoredETagAcrossCopies(t *testing.T) {
	t.Parallel()
	dead := backendtest.NewInMemory()
	dead.GetErr = context.DeadlineExceeded
	alive := backendtest.NewInMemory()
	alive.Put("key", &backendtest.Object{Data: []byte("bytes"), ETag: `"replica-own-etag"`})

	locs := []core.ObjectLocation{
		{ObjectKey: "key", BackendName: "b1", SizeBytes: 5, CreatedAt: lmCreatedAt,
			Identity: &core.ObjectIdentity{ETag: `"object-etag"`, ContentType: "text/plain"}},
		{ObjectKey: "key", BackendName: "b2", SizeBytes: 5, CreatedAt: lmCreatedAt,
			Identity: &core.ObjectIdentity{ETag: `"object-etag"`, ContentType: "text/plain"}},
	}
	store := locationsStore(t, locs, nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": dead, "b2": alive}, &fleetOpts{Order: []string{"b1", "b2"}})

	res, err := mgr.GetObject(context.Background(), "key", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer res.Body.Close()

	if res.ETag != `"object-etag"` {
		t.Errorf("etag = %q, want the object's own after failing over to the replica", res.ETag)
	}
}
