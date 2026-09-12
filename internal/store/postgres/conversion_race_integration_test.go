// -------------------------------------------------------------------------------
// Stored-Form Rewrite Concurrency Tests
//
// Author: Alex Freidah
//
// A bulk conversion reads a copy, transforms it, and writes it back. A client
// writing the same key in between used to be invisible to it: the commit was an
// unconditional UPDATE, so the pass stamped the row with the form of the version
// it had read and the newer write was lost or the row was left describing bytes
// that no longer existed.
//
// These assert the predicate that closes it. Each commits against an etag the
// row no longer reports and expects the sentinel rather than a write, and the
// positive cases pin that an unchanged copy still converts - a predicate that
// refused everything would pass the negative tests alone.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// recordWithEtag stores one copy carrying the given etag, which is what a
// client write leaves behind and what the conversions predicate on.
func recordWithEtag(t *testing.T, s *Store, key, etag string, size int64) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key:      key,
		Copies:   []core.ObjectCopy{{Backend: "backend-a"}},
		Size:     size,
		Identity: &core.ObjectIdentity{ETag: etag},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStoreInt_MarkObjectEncrypted_RefusesAChangedCopy is the regression test
// for the lost write: the pass read one version, a client replaced it, and the
// commit must not describe the version the pass held.
func TestStoreInt_MarkObjectEncrypted_RefusesAChangedCopy(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "encrypt-race")

	recordWithEtag(t, s, key, "etag-v1", 100)

	// The client's write, landing after the pass read the copy.
	recordWithEtag(t, s, key, "etag-v2", 140)

	u := encUpdate(key, 100, 200)
	u.ExpectedEtag = "etag-v1"
	err := s.MarkObjectEncrypted(ctx, u)

	if !errors.Is(err, core.ErrCopyChanged) {
		t.Fatalf("MarkObjectEncrypted = %v, want ErrCopyChanged", err)
	}

	locs, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("locations = %d, want 1", len(locs))
	}
	if locs[0].Encrypted {
		t.Error("the row was stamped encrypted despite the copy having changed")
	}
	if locs[0].SizeBytes != 140 {
		t.Errorf("size_bytes = %d, want the client's 140", locs[0].SizeBytes)
	}
}

// TestStoreInt_MarkObjectEncrypted_ConvertsAnUnchangedCopy pins the positive
// case, so the predicate is not simply refusing everything.
func TestStoreInt_MarkObjectEncrypted_ConvertsAnUnchangedCopy(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "encrypt-unchanged")

	recordWithEtag(t, s, key, "etag-v1", 100)

	u := encUpdate(key, 100, 200)
	u.ExpectedEtag = "etag-v1"
	if err := s.MarkObjectEncrypted(ctx, u); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}

	locs, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if !locs[0].Encrypted {
		t.Error("an unchanged copy was not converted")
	}
}

// TestStoreInt_MarkObjectDecrypted_RefusesAChangedCopy covers the same window
// on the decrypt direction.
func TestStoreInt_MarkObjectDecrypted_RefusesAChangedCopy(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "decrypt-race")

	recordWithEtag(t, s, key, "etag-v1", 200)
	recordWithEtag(t, s, key, "etag-v2", 160)

	err := s.MarkObjectDecrypted(ctx, &core.DecryptedUpdate{
		ObjectKey:     key,
		BackendName:   "backend-a",
		PlaintextSize: 100,
		ExpectedEtag:  "etag-v1",
	})

	if !errors.Is(err, core.ErrCopyChanged) {
		t.Fatalf("MarkObjectDecrypted = %v, want ErrCopyChanged", err)
	}
}

// TestStoreInt_MarkObjectCompressed_RefusesAChangedCopy covers the compression
// direction, which commits through a different statement again.
func TestStoreInt_MarkObjectCompressed_RefusesAChangedCopy(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "compress-race")

	recordWithEtag(t, s, key, "etag-v1", 100)
	recordWithEtag(t, s, key, "etag-v2", 180)

	err := s.MarkObjectCompressed(ctx, &core.CompressedUpdate{
		ObjectKey:     key,
		BackendName:   "backend-a",
		Algorithm:     "zstd",
		Level:         "default",
		FormatVersion: 1,
		SizeBytes:     60,
		LogicalSize:   100,
		ExpectedEtag:  "etag-v1",
	}, 100)

	if !errors.Is(err, core.ErrCopyChanged) {
		t.Fatalf("MarkObjectCompressed = %v, want ErrCopyChanged", err)
	}
}

// TestStoreInt_Conversion_RefusesACopyThatGainedAnEtag pins the null-safe half
// of the predicate. A row recorded before identities were kept carries no etag,
// and a client write gives it one - which has to read as changed rather than
// matching on NULL.
func TestStoreInt_Conversion_RefusesACopyThatGainedAnEtag(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "etag-gained")

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key:    key,
		Copies: []core.ObjectCopy{{Backend: "backend-a"}},
		Size:   100,
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	recordWithEtag(t, s, key, "etag-v2", 100)

	u := encUpdate(key, 100, 200)
	u.ExpectedEtag = ""
	if err := s.MarkObjectEncrypted(ctx, u); !errors.Is(err, core.ErrCopyChanged) {
		t.Fatalf("MarkObjectEncrypted = %v, want ErrCopyChanged", err)
	}
}

// TestStoreInt_Conversion_ConvertsACopyWithNoEtag pins the other side of that:
// a copy that never had one still converts, so rows predating identity are not
// stranded unconvertible.
func TestStoreInt_Conversion_ConvertsACopyWithNoEtag(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "etag-absent")

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key:    key,
		Copies: []core.ObjectCopy{{Backend: "backend-a"}},
		Size:   100,
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	u := encUpdate(key, 100, 200)
	u.ExpectedEtag = ""
	if err := s.MarkObjectEncrypted(ctx, u); err != nil {
		t.Fatalf("MarkObjectEncrypted: %v", err)
	}
}
