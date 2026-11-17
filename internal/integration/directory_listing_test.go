// -------------------------------------------------------------------------------
// Directory Listing Integration Tests - File Browser Pages on PostgreSQL
//
// Author: Alex Freidah
//
// Drives the file browser's page builder against a real PostgreSQL container,
// so the delimited walk, the folder totals and the backends lookup are covered
// together. Verifies that a page:
//   - sums physical bytes across replica rows for a folder's totals
//   - reports the full sorted set of backends a replicated file lives on
//   - pages folders and files behind one cursor, listing each once
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// dirTestPrefix builds a per-test directory prefix, so one test's keys stay out
// of another's roll-ups.
func dirTestPrefix(t *testing.T, label string) string {
	t.Helper()
	return label + "-" + t.Name() + "/"
}

// browser builds the file browser's operation over the PostgreSQL store. Browse
// reads only the store, so the object manager is a mock nothing calls.
func browser(t *testing.T) *ops.Objects {
	t.Helper()
	return ops.NewObjects(ops.ObjectsDeps{
		Objects: opstest.NewMockObjectAPI(gomock.NewController(t)),
		Store:   testStore,
		Buckets: provisioning.NewDeclared(),
	})
}

// recordAt records key on backend with size bytes and deletes it when the test
// ends.
func recordAt(t *testing.T, ctx context.Context, key, backend string, size int64) {
	t.Helper()
	if _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: backend}}, Size: size}); err != nil {
		t.Fatalf("RecordObject(%s): %v", key, err)
	}
	t.Cleanup(func() { _, _ = testStore.DeleteObject(context.Background(), key) })
}

// browse returns one page of the file browser under prefix.
func browse(t *testing.T, ctx context.Context, prefix, cursor string, maxKeys int) *core.DirectoryListResult {
	t.Helper()
	page, err := browser(t).Browse(ctx, prefix, cursor, maxKeys)
	if err != nil {
		t.Fatalf("Browse(%s, %q): %v", prefix, cursor, err)
	}
	return page
}

// findEntryByName returns the first DirEntry whose Name matches; nil when
// the entry is absent (callers fail the test instead of dereferencing).
func findEntryByName(entries []core.DirEntry, name string) *core.DirEntry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestPgBrowse_FileRowReplicated asserts a replicated file's row exposes every
// backend it lives on as a sorted slice and reports the size the client wrote.
func TestPgBrowse_FileRowReplicated(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	prefix := dirTestPrefix(t, "dirtest-replicated")
	key := prefix + "file.txt"

	recordAt(t, ctx, key, "minio-1", 100)
	placeReplica(t, ctx, key, "minio-1", "minio-2")

	got := findEntryByName(browse(t, ctx, prefix, "", 100).Entries, key)
	if got == nil {
		t.Fatalf("missing entry for %q", key)
	}
	if got.TotalSize != 100 {
		t.Errorf("TotalSize = %d, want 100 (the object's size, not the replica sum)", got.TotalSize)
	}
	if want := []string{"minio-1", "minio-2"}; !reflect.DeepEqual(got.Backends, want) {
		t.Errorf("Backends = %v, want %v (sorted)", got.Backends, want)
	}
}

// TestPgBrowse_FolderRollupPhysicalBytes asserts a folder's totals sum physical
// bytes across replicas (matching Storage Summary semantics) and count
// distinct object keys.
func TestPgBrowse_FolderRollupPhysicalBytes(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	parent := dirTestPrefix(t, "dirtest-rollup")
	repKey := parent + "child/replicated.txt"

	recordAt(t, ctx, repKey, "minio-1", 100)
	placeReplica(t, ctx, repKey, "minio-1", "minio-2")
	recordAt(t, ctx, parent+"child/single.txt", "minio-1", 50)

	got := findEntryByName(browse(t, ctx, parent, "", 100).Entries, parent+"child/")
	if got == nil || !got.IsDir {
		t.Fatalf("missing folder entry for %q", parent+"child/")
	}
	if got.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2 (distinct object keys)", got.FileCount)
	}
	if got.TotalSize != 250 {
		t.Errorf("TotalSize = %d, want 250 (physical: 100+100+50)", got.TotalSize)
	}
}

// TestPgBrowse_PagesFoldersOnce asserts a folder with more children than one
// page holds pages its folders and files behind one cursor, each listed once
// and each folder totalled on the page that lists it.
func TestPgBrowse_PagesFoldersOnce(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	prefix := dirTestPrefix(t, "dirtest-paged")
	for _, key := range []string{"a/1", "b/1", "c/1", "f"} {
		recordAt(t, ctx, prefix+key, "minio-1", 10)
	}

	var names []string
	cursor := ""
	for range 10 {
		page := browse(t, ctx, prefix, cursor, 2)
		for _, e := range page.Entries {
			names = append(names, e.Name)
			if e.IsDir && e.FileCount != 1 {
				t.Errorf("%s FileCount = %d, want 1", e.Name, e.FileCount)
			}
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	want := []string{prefix + "a/", prefix + "b/", prefix + "c/", prefix + "f"}
	if !slices.Equal(names, want) {
		t.Errorf("entries = %q, want %q", names, want)
	}
}
