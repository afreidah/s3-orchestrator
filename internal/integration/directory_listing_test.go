// -------------------------------------------------------------------------------
// Directory Listing Integration Tests - Replication-Aware Aggregation
//
// Author: Alex Freidah
//
// Mirrors the SQLite-side replication-semantics tests against a real
// PostgreSQL container so the production code path in
// internal/store/objects.go is covered. Verifies that the dashboard
// directory tree:
//   - sums physical bytes across replica rows for directory roll-ups
//   - reports the full sorted set of backends a replicated file lives on
//   - leaves single-replica files reporting their lone backend
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// dirTestPrefix builds a per-test directory prefix free of LIKE special
// characters (% _ \) so these replication-semantics tests stay focused on
// roll-up behaviour; the LIKE-escaping path has its own coverage in
// TestPgListDirectoryChildren_UnderscorePrefix.
func dirTestPrefix(t *testing.T, label string) string {
	t.Helper()
	clean := strings.ReplaceAll(t.Name(), "_", "-")
	return label + "-" + clean + "/"
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestPgListDirectoryChildren_FileRowReplicated asserts that on PostgreSQL
// a replicated file's row exposes every backend it lives on as a sorted
// slice and reports the logical (single-replica) size.
func TestPgListDirectoryChildren_FileRowReplicated(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	prefix := dirTestPrefix(t, "dirtest-replicated")
	key := prefix + "file.txt"

	if _, _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "minio-1", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, _, err := testStore.RecordReplica(ctx, key, "minio-2", "minio-1"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	t.Cleanup(func() {
		_, _, _ = testStore.DeleteObject(context.Background(), key)
	})

	result, err := testStore.ListDirectoryChildren(ctx, prefix, "", 100)
	if err != nil {
		t.Fatalf("ListDirectoryChildren: %v", err)
	}

	got := findEntryByName(result.Entries, key)
	if got == nil {
		t.Fatalf("missing entry for %q in %+v", key, result.Entries)
	}
	if got.TotalSize != 100 {
		t.Errorf("TotalSize = %d, want 100 (logical, single replica)", got.TotalSize)
	}
	if want := []string{"minio-1", "minio-2"}; !reflect.DeepEqual(got.Backends, want) {
		t.Errorf("Backends = %v, want %v (sorted)", got.Backends, want)
	}
}

// TestPgListDirectoryChildren_FileRowSingle asserts that a single-replica
// file rolls up with its one backend in a one-element slice.
func TestPgListDirectoryChildren_FileRowSingle(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	prefix := dirTestPrefix(t, "dirtest-single")
	key := prefix + "file.txt"

	if _, _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "minio-1", Size: 50}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	t.Cleanup(func() {
		_, _, _ = testStore.DeleteObject(context.Background(), key)
	})

	result, err := testStore.ListDirectoryChildren(ctx, prefix, "", 100)
	if err != nil {
		t.Fatalf("ListDirectoryChildren: %v", err)
	}

	got := findEntryByName(result.Entries, key)
	if got == nil {
		t.Fatalf("missing entry for %q", key)
	}
	if got.TotalSize != 50 {
		t.Errorf("TotalSize = %d, want 50", got.TotalSize)
	}
	if want := []string{"minio-1"}; !reflect.DeepEqual(got.Backends, want) {
		t.Errorf("Backends = %v, want %v", got.Backends, want)
	}
}

// TestPgListDirectoryChildren_DirRollupPhysicalBytes asserts the directory
// roll-up at the parent prefix sums physical bytes across replicas
// (matching Storage Summary semantics) and counts distinct object keys.
func TestPgListDirectoryChildren_DirRollupPhysicalBytes(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	parent := dirTestPrefix(t, "dirtest-rollup")
	repKey := parent + "child/replicated.txt"
	singleKey := parent + "child/single.txt"

	if _, _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: repKey, Backend: "minio-1", Size: 100}); err != nil {
		t.Fatalf("RecordObject(replicated): %v", err)
	}
	if _, _, err := testStore.RecordReplica(ctx, repKey, "minio-2", "minio-1"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	if _, _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: singleKey, Backend: "minio-1", Size: 50}); err != nil {
		t.Fatalf("RecordObject(single): %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _, _ = testStore.DeleteObject(bg, repKey)
		_, _, _ = testStore.DeleteObject(bg, singleKey)
	})

	result, err := testStore.ListDirectoryChildren(ctx, parent, "", 100)
	if err != nil {
		t.Fatalf("ListDirectoryChildren: %v", err)
	}

	got := findEntryByName(result.Entries, parent+"child/")
	if got == nil || !got.IsDir {
		t.Fatalf("missing directory entry for %q in %+v", parent+"child/", result.Entries)
	}
	if got.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2 (distinct object keys)", got.FileCount)
	}
	if got.TotalSize != 250 {
		t.Errorf("TotalSize = %d, want 250 (physical: 100+100+50)", got.TotalSize)
	}
}

// TestPgListDirectoryChildren_UnderscorePrefix is the regression test for the
// bug where a directory whose name contained an underscore listed empty. The
// caller LIKE-escapes the prefix ('_' -> '\_'), which lengthens it; reusing
// that escaped length as the child-name substring offset cut one character too
// deep, so every child name was mangled, missed the file lookup, and was
// dropped. Both a file child and a subdirectory child must surface.
func TestPgListDirectoryChildren_UnderscorePrefix(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	// The underscore is the whole point, so build the prefix directly rather
	// than via dirTestPrefix (which strips LIKE specials).
	prefix := "dirtest_underscore/"
	fileKey := prefix + "file.txt"
	subKey := prefix + "sub/inner.txt"

	if _, _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: fileKey, Backend: "minio-1", Size: 100}); err != nil {
		t.Fatalf("RecordObject(file): %v", err)
	}
	if _, _, err := testStore.RecordObject(ctx, &core.RecordObjectRequest{Key: subKey, Backend: "minio-1", Size: 50}); err != nil {
		t.Fatalf("RecordObject(sub): %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _, _ = testStore.DeleteObject(bg, fileKey)
		_, _, _ = testStore.DeleteObject(bg, subKey)
	})

	result, err := testStore.ListDirectoryChildren(ctx, prefix, "", 100)
	if err != nil {
		t.Fatalf("ListDirectoryChildren: %v", err)
	}

	file := findEntryByName(result.Entries, fileKey)
	if file == nil {
		t.Fatalf("file child missing under underscore prefix %q; got %+v", prefix, result.Entries)
	}
	if file.IsDir || file.TotalSize != 100 {
		t.Errorf("file entry = %+v, want IsDir=false TotalSize=100", file)
	}

	dir := findEntryByName(result.Entries, prefix+"sub/")
	if dir == nil || !dir.IsDir {
		t.Fatalf("subdirectory child missing under underscore prefix; got %+v", result.Entries)
	}
	if dir.FileCount != 1 {
		t.Errorf("subdir FileCount = %d, want 1", dir.FileCount)
	}
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
