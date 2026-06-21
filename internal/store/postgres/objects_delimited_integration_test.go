// -------------------------------------------------------------------------------
// Postgres ListObjectsDelimited - Integration Tests
//
// Author: Alex Freidah
//
// Cross-engine parity for the delimiter loose-index-scan: the same scenarios the
// SQLite unit tests cover (grouping + leaves, duplicate-free pagination) run
// here against real Postgres so the recursive-CTE query and COLLATE "C" byte
// ordering produce identical CommonPrefixes, leaf objects, and pagination tokens
// on both engines.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"fmt"
	"testing"
)

// TestStoreInt_ListObjectsDelimited_GroupsAndLeaves verifies keys fold into
// CommonPrefixes at the first delimiter after the prefix while leaf objects pass
// through with their location columns.
func TestStoreInt_ListObjectsDelimited_GroupsAndLeaves(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	prefix := uniqueKey(t, "") // t.Name()+"/" isolates this test's keyspace

	keys := []string{
		prefix + "a/1.txt", prefix + "a/2.txt",
		prefix + "b/1.txt",
		prefix + "file1.txt", prefix + "file2.txt",
	}
	for _, k := range keys {
		if _, err := s.RecordObject(ctx, k, "backend-a", 10, nil); err != nil {
			t.Fatalf("RecordObject(%s): %v", k, err)
		}
		defer func(k string) { _, _ = s.DeleteObject(ctx, k) }(k)
	}

	res, err := s.ListObjectsDelimited(ctx, prefix, "/", "", 1000)
	if err != nil {
		t.Fatalf("ListObjectsDelimited: %v", err)
	}

	wantCP := []string{prefix + "a/", prefix + "b/"}
	if len(res.CommonPrefixes) != 2 || res.CommonPrefixes[0] != wantCP[0] || res.CommonPrefixes[1] != wantCP[1] {
		t.Errorf("CommonPrefixes = %v, want %v", res.CommonPrefixes, wantCP)
	}
	if len(res.Objects) != 2 || res.Objects[0].ObjectKey != prefix+"file1.txt" || res.Objects[1].ObjectKey != prefix+"file2.txt" {
		t.Errorf("Objects = %+v, want file1.txt,file2.txt", res.Objects)
	}
	if res.Objects[0].BackendName != "backend-a" || res.Objects[0].SizeBytes != 10 {
		t.Errorf("leaf location not populated: %+v", res.Objects[0])
	}
	if res.IsTruncated {
		t.Error("IsTruncated = true, want false")
	}
}

// TestStoreInt_ListObjectsDelimited_PaginationNoDuplicate verifies paginating
// across calls via the continuation token emits every CommonPrefix exactly once.
func TestStoreInt_ListObjectsDelimited_PaginationNoDuplicate(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	prefix := uniqueKey(t, "")

	for g := range 4 {
		for k := range 3 {
			key := fmt.Sprintf("%sg%d/k%d.txt", prefix, g, k)
			if _, err := s.RecordObject(ctx, key, "backend-a", 1, nil); err != nil {
				t.Fatalf("RecordObject(%s): %v", key, err)
			}
			defer func(k string) { _, _ = s.DeleteObject(ctx, k) }(key)
		}
	}

	seen := map[string]int{}
	after := ""
	for range 11 {
		res, err := s.ListObjectsDelimited(ctx, prefix, "/", after, 2)
		if err != nil {
			t.Fatalf("ListObjectsDelimited: %v", err)
		}
		for _, cp := range res.CommonPrefixes {
			seen[cp]++
		}
		if !res.IsTruncated {
			break
		}
		after = res.NextContinuationToken
	}

	for g := range 4 {
		cp := fmt.Sprintf("%sg%d/", prefix, g)
		if seen[cp] != 1 {
			t.Errorf("CommonPrefix %q emitted %d times, want 1", cp, seen[cp])
		}
	}
	if len(seen) != 4 {
		t.Errorf("distinct prefixes = %d, want 4: %v", len(seen), seen)
	}
}
