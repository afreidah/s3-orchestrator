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

// dirTestPrefix builds a per-test directory prefix that contains no LIKE
// special characters (% _ \) so it survives the SQL escape pass without
// confusing length-based substring math in GetDirectoryStats.
func dirTestPrefix(t *testing.T, label string) string {
	t.Helper()
	clean := strings.ReplaceAll(t.Name(), "_", "-")
	return label + "-" + clean + "/"
}

// TestPgListDirectoryChildren_FileRowReplicated asserts that on PostgreSQL
// a replicated file's row exposes every backend it lives on as a sorted
// slice and reports the logical (single-replica) size.
func TestPgListDirectoryChildren_FileRowReplicated(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	prefix := dirTestPrefix(t, "dirtest-replicated")
	key := prefix + "file.txt"

	if _, err := testStore.RecordObject(ctx, key, "minio-1", 100, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, err := testStore.RecordReplica(ctx, key, "minio-2", "minio-1", 100); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testStore.DeleteObject(context.Background(), key)
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

	if _, err := testStore.RecordObject(ctx, key, "minio-1", 50, nil); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testStore.DeleteObject(context.Background(), key)
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

	if _, err := testStore.RecordObject(ctx, repKey, "minio-1", 100, nil); err != nil {
		t.Fatalf("RecordObject(replicated): %v", err)
	}
	if _, err := testStore.RecordReplica(ctx, repKey, "minio-2", "minio-1", 100); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	if _, err := testStore.RecordObject(ctx, singleKey, "minio-1", 50, nil); err != nil {
		t.Fatalf("RecordObject(single): %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = testStore.DeleteObject(bg, repKey)
		_, _ = testStore.DeleteObject(bg, singleKey)
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
