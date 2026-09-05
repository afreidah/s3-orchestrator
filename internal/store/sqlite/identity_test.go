// -------------------------------------------------------------------------------
// SQLite - Object Identity Column Tests
//
// Author: Alex Freidah
//
// The three rules the identity columns are written under: a write records what
// it computed, a later read fills only what is still unknown, and both apply to
// every copy of the key rather than the one that happened to answer. The last
// one is the whole point - a per-copy value is what lets a failover change the
// ETag under a conditional request.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// identityOf reads back the identity recorded for one copy.
func identityOf(t *testing.T, s *Store, key, backendName string) *core.ObjectIdentity {
	t.Helper()
	locs, err := s.GetAllObjectLocations(context.Background(), key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations(%s): %v", key, err)
	}
	for i := range locs {
		if locs[i].BackendName == backendName {
			return locs[i].Identity
		}
	}
	t.Fatalf("no copy of %s on %s", key, backendName)
	return nil
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestRecordObject_RoundTripsIdentity covers the write path: what PutObject
// computed is what a read gets back.
func TestRecordObject_RoundTripsIdentity(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100,
		Identity: &core.ObjectIdentity{
			ETag:         `"written"`,
			ContentType:  "text/plain",
			UserMetadata: map[string]string{"owner": "team-a"},
		},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	got := identityOf(t, s, "bucket/k", "backend-a")
	if got == nil {
		t.Fatal("identity = nil, want the one recorded")
	}
	if got.ETag != `"written"` || got.ContentType != "text/plain" {
		t.Errorf("identity = %+v, want the recorded etag and content type", got)
	}
	if got.UserMetadata["owner"] != "team-a" {
		t.Errorf("user metadata = %v, want the recorded map", got.UserMetadata)
	}
}

// TestRecordObjectIdentity_FillsOnlyWhatIsUnknown pins the COALESCE rule: a
// value computed from the client's own bytes outranks anything a backend
// reports later about the bytes as stored.
func TestRecordObjectIdentity_FillsOnlyWhatIsUnknown(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100,
		Identity: &core.ObjectIdentity{ETag: `"original"`},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	if err := s.RecordObjectIdentity(ctx, "bucket/k", &core.ObjectIdentity{
		ETag:         `"from-backend"`,
		ContentType:  "application/json",
		UserMetadata: map[string]string{"k": "v"},
	}); err != nil {
		t.Fatalf("RecordObjectIdentity: %v", err)
	}

	got := identityOf(t, s, "bucket/k", "backend-a")
	if got.ETag != `"original"` {
		t.Errorf("etag = %q, want the recorded one kept", got.ETag)
	}
	if got.ContentType != "application/json" {
		t.Errorf("content type = %q, want the previously unknown column filled", got.ContentType)
	}
	if got.UserMetadata["k"] != "v" {
		t.Errorf("user metadata = %v, want the previously unknown column filled", got.UserMetadata)
	}
}

// TestRecordObjectIdentity_AppliesToEveryCopy is the divergence guard: a read
// that learned an identity writes it for the whole key, so the next read gets
// the same answer whichever copy it reaches.
func TestRecordObjectIdentity_AppliesToEveryCopy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	mustRecordObject(t, s, "bucket/k", "backend-a", 100)
	if _, _, err := s.RecordReplica(ctx, "bucket/k", "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}

	if err := s.RecordObjectIdentity(ctx, "bucket/k", &core.ObjectIdentity{ETag: `"learned"`}); err != nil {
		t.Fatalf("RecordObjectIdentity: %v", err)
	}

	for _, backendName := range []string{"backend-a", "backend-b"} {
		got := identityOf(t, s, "bucket/k", backendName)
		if got == nil || got.ETag != `"learned"` {
			t.Errorf("copy on %s has identity %+v, want the learned etag", backendName, got)
		}
	}
}

// TestListObjects_CarriesTheStoredETag pins that a listing reports the
// object's own ETag: a Contents entry carries one, and deriving it from the
// serving copy is the divergence the column exists to end. An object that has
// not learned one reports none rather than something else's digest.
func TestListObjects_CarriesTheStoredETag(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/with-etag", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 10,
		Identity: &core.ObjectIdentity{ETag: `"listed"`},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	mustRecordObject(t, s, "bucket/without-etag", "backend-a", 20)

	page, err := s.ListObjects(ctx, "bucket/", "", 10)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	byKey := map[string]core.ObjectLocation{}
	for _, o := range page.Objects {
		byKey[o.ObjectKey] = o
	}
	if got := byKey["bucket/with-etag"].Identity; got == nil || got.ETag != `"listed"` {
		t.Errorf("identity = %+v, want the recorded etag", got)
	}
	if got := byKey["bucket/without-etag"].Identity; got != nil {
		t.Errorf("identity = %+v, want none for an object that has not learned an etag", got)
	}
}

// TestListObjectsDelimited_CarriesTheStoredETag covers the same contract on
// the delimiter path, whose leaf rows come from a different query.
func TestListObjectsDelimited_CarriesTheStoredETag(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/leaf.txt", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 10,
		Identity: &core.ObjectIdentity{ETag: `"leaf-etag"`},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	page, err := s.ListObjectsDelimited(ctx, "bucket/", "/", "", 10)
	if err != nil {
		t.Fatalf("ListObjectsDelimited: %v", err)
	}
	for _, o := range page.Objects {
		if o.ObjectKey != "bucket/leaf.txt" {
			continue
		}
		if o.Identity == nil || o.Identity.ETag != `"leaf-etag"` {
			t.Errorf("identity = %+v, want the recorded etag", o.Identity)
		}
		return
	}
	t.Fatalf("leaf object missing from the delimited page: %+v", page.Objects)
}

// TestRecordObjectIdentity_NilIsANoOp covers the caller that has nothing to
// record, which must not clear what is already there.
func TestRecordObjectIdentity_NilIsANoOp(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: "bucket/k", Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100,
		Identity: &core.ObjectIdentity{ETag: `"kept"`},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	if err := s.RecordObjectIdentity(ctx, "bucket/k", nil); err != nil {
		t.Fatalf("RecordObjectIdentity(nil): %v", err)
	}

	if got := identityOf(t, s, "bucket/k", "backend-a"); got.ETag != `"kept"` {
		t.Errorf("etag = %q, want it untouched by a nil identity", got.ETag)
	}
}
