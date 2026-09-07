// -------------------------------------------------------------------------------
// Provisioning - Store Read Tests
//
// Author: Alex Freidah
//
// Covers the read: every table reaching the snapshot, and a failure on any one
// of them failing the whole read so nothing merges against a partial picture.
// -------------------------------------------------------------------------------

package provisioning

import (
	"context"
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// fakeReader answers each listing from a fixed snapshot, failing on the table
// named in failOn.
type fakeReader struct {
	snap   Snapshot
	failOn string
	err    error
}

func (f *fakeReader) fail(table string) error {
	if f.failOn == table {
		return f.err
	}
	return nil
}

func (f *fakeReader) ListBuckets(context.Context) ([]core.Bucket, error) {
	return f.snap.Buckets, f.fail("buckets")
}

func (f *fakeReader) ListUsers(context.Context) ([]core.User, error) {
	return f.snap.Users, f.fail("users")
}

func (f *fakeReader) ListCredentials(context.Context) ([]core.Credential, error) {
	return f.snap.Credentials, f.fail("credentials")
}

func (f *fakeReader) ListGrants(context.Context) ([]core.Grant, error) {
	return f.snap.Grants, f.fail("grants")
}

// TestLoad_ReadsEveryTable verifies each listing reaches the snapshot, so the
// merge sees the whole of what the store holds.
func TestLoad_ReadsEveryTable(t *testing.T) {
	t.Parallel()

	r := &fakeReader{snap: Snapshot{
		Buckets:     []core.Bucket{{Name: "b"}},
		Users:       []core.User{{ID: "u1"}},
		Credentials: []core.Credential{{AccessKeyID: "AK"}},
		Grants:      []core.Grant{{UserID: "u1", BucketName: "b"}},
	}}

	s, err := Load(context.Background(), r)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Buckets) != 1 || len(s.Users) != 1 || len(s.Credentials) != 1 || len(s.Grants) != 1 {
		t.Errorf("snapshot = %+v, want one row in each table", s)
	}
}

// TestLoad_ListingFailurePropagates verifies a failure reading any one table
// fails the whole read.
func TestLoad_ListingFailurePropagates(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	for _, table := range []string{"buckets", "users", "credentials", "grants"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			r := &fakeReader{failOn: table, err: boom}
			if _, err := Load(context.Background(), r); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want a wrap of boom", err)
			}
		})
	}
}

// TestLoadMerged_FoldsConfigIn verifies the combined entry point produces the
// same union both the registry and an operator's listing are built from.
func TestLoadMerged_FoldsConfigIn(t *testing.T) {
	t.Parallel()

	r := &fakeReader{snap: Snapshot{Buckets: []core.Bucket{{Name: "from-store"}}}}

	v, err := LoadMerged(context.Background(), r, []config.BucketConfig{{Name: "from-config"}})
	if err != nil {
		t.Fatalf("LoadMerged: %v", err)
	}
	if got := bucketNames(v.Buckets); len(got) != 2 {
		t.Fatalf("merged buckets = %v, want both sources", got)
	}
}

// TestLoadMerged_ReadFailurePropagates verifies a store that cannot be read
// fails rather than merging config alone, which would answer 403 to callers the
// store entitles.
func TestLoadMerged_ReadFailurePropagates(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	r := &fakeReader{failOn: "users", err: boom}
	if _, err := LoadMerged(context.Background(), r, nil); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want a wrap of boom", err)
	}
}
