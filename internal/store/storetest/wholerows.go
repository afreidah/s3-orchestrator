// -------------------------------------------------------------------------------
// Whole-Row Read Check
//
// Author: Alex Freidah
//
// Every store read that returns copies reads the whole object_locations row, so
// a copy is the same value whichever read produced it: the read path's lookup,
// the replication scans, the scrub queue or a pass's listing. Both engines run
// this check against a real store, which keeps them from drifting apart on
// which fields a read fills.
// -------------------------------------------------------------------------------

package storetest

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// WholeRowReader is the store surface CheckWholeRowReads drives, which both
// engines satisfy.
type WholeRowReader interface {
	LocationLister
	RecordObject(ctx context.Context, req *core.RecordObjectRequest) ([]core.CleanupItem, error)
	ImportObject(ctx context.Context, req *core.ImportObjectRequest) (core.ImportOutcome, error)
	GetUnderReplicatedObjects(ctx context.Context, factor, limit int, excluded []string) ([]core.ObjectLocation, error)
	GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error)
	GetLeastRecentlyScrubbedObjects(ctx context.Context, limit int, backends []string, scrubbedBefore time.Time) ([]core.ObjectLocation, error)
	ListCopiesByForm(ctx context.Context, f *core.FormFilter, limit int, after core.Cursor) ([]core.ObjectLocation, error)
}

// CheckWholeRowReads records key with a managed copy on managed and imports an
// unmanaged copy of it on unmanaged, then fails unless every batch read returns
// each copy exactly as the read path does. Both backends must have room for 64
// bytes and hold nothing else.
func CheckWholeRowReads(t *testing.T, s WholeRowReader, key, managed, unmanaged string) {
	t.Helper()
	ctx := context.Background()
	c := rowCheck{key: key, want: seedWholeRows(t, s, key, managed, unmanaged)}

	under, err := s.GetUnderReplicatedObjects(ctx, 3, math.MaxInt32, nil)
	c.read(t, "GetUnderReplicatedObjects", 2, under, err)
	over, err := s.GetOverReplicatedObjects(ctx, 0, math.MaxInt32)
	c.read(t, "GetOverReplicatedObjects", 2, over, err)
	scrub, err := s.GetLeastRecentlyScrubbedObjects(ctx, 10, []string{managed}, time.Now().Add(time.Hour))
	c.read(t, "GetLeastRecentlyScrubbedObjects", 1, scrub, err)
	for _, backend := range []string{managed, unmanaged} {
		listed, err := s.ListCopiesByForm(ctx, &core.FormFilter{Backend: backend}, 10, core.Cursor{})
		c.read(t, "ListCopiesByForm", 1, listed, err)
	}
}

// seedWholeRows writes the two copies and returns them as the read path reads
// them, keyed by backend, after checking that read carries the columns the
// batch reads are compared on.
func seedWholeRows(t *testing.T, s WholeRowReader, key, managed, unmanaged string) map[string]core.ObjectLocation {
	t.Helper()
	ctx := context.Background()
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key:  key,
		Size: 64,
		Form: &core.StoredForm{
			Encrypted: true, EncryptionKey: []byte{1}, KeyID: "k1", PlaintextSize: 60, ContentHash: "sha256:00",
		},
		Identity: core.NewObjectIdentity(`"e1"`, "text/plain", map[string]string{"a": "b"}),
		Copies:   []core.ObjectCopy{{Backend: managed, StorageKey: key + "!managed"}},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if out, err := s.ImportObject(ctx, &core.ImportObjectRequest{
		Key: key, Backend: unmanaged, Size: 64, Unmanaged: true, Form: &core.StoredForm{},
	}); err != nil || out != core.ImportInserted {
		t.Fatalf("ImportObject = %v, %v; want inserted", out, err)
	}

	want := map[string]core.ObjectLocation{
		managed:   *LocationOn(t, s, key, managed),
		unmanaged: *LocationOn(t, s, key, unmanaged),
	}
	if m := want[managed]; m.Identity == nil || m.ContentHash == "" || m.Unmanaged {
		t.Fatalf("read path lost columns of the managed copy: %+v", m)
	}
	if !want[unmanaged].Unmanaged {
		t.Fatal("read path reports the imported copy as managed")
	}
	return want
}

// rowCheck compares what a batch read returns for key with the copies the read
// path returned, keyed by backend.
type rowCheck struct {
	key  string
	want map[string]core.ObjectLocation
}

// read fails unless locs holds wantCopies copies of the key, each equal to the
// read path's copy on its backend.
func (c rowCheck) read(t *testing.T, name string, wantCopies int, locs []core.ObjectLocation, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	seen := 0
	for i := range locs {
		if locs[i].ObjectKey != c.key {
			continue
		}
		seen++
		if want := c.want[locs[i].BackendName]; !reflect.DeepEqual(locs[i], want) {
			t.Errorf("%s copy on %s =\n %+v\nwant\n %+v", name, locs[i].BackendName, locs[i], want)
		}
	}
	if seen != wantCopies {
		t.Errorf("%s returned %d copies of %s, want %d", name, seen, c.key, wantCopies)
	}
}
