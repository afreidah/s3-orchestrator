// -------------------------------------------------------------------------------
// SQLite ListObjectsDelimited - Tests
//
// Author: Alex Freidah
//
// Validates the loose-index-scan recursive CTE: keys collapse into
// CommonPrefixes at the first delimiter after the prefix, leaf objects pass
// through with their location columns, and pagination via the skip-bound
// continuation token emits every prefix exactly once.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// cpList runs ListObjectsDelimited and returns CommonPrefixes + leaf keys.
func leafKeys(t *testing.T, s *Store, prefix, delim, after string, maxKeys int) (cps, leaves []string) {
	t.Helper()
	res, err := s.ListObjectsDelimited(context.Background(), prefix, delim, after, maxKeys)
	if err != nil {
		t.Fatalf("ListObjectsDelimited: %v", err)
	}
	cps = res.CommonPrefixes
	for i := range res.Objects {
		leaves = append(leaves, res.Objects[i].ObjectKey)
	}
	return cps, leaves
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestListObjectsDelimited_GroupsAndLeaves verifies folding into CommonPrefixes
// while leaf objects (no delimiter after the prefix) pass through.
func TestListObjectsDelimited_GroupsAndLeaves(t *testing.T) {
	s := newTestStore(t)
	for _, k := range []string{"p/a/1.txt", "p/a/2.txt", "p/b/1.txt", "p/file1.txt", "p/file2.txt", "other/x.txt"} {
		mustRecordObject(t, s, k, "backend-a", 10)
	}

	res, err := s.ListObjectsDelimited(context.Background(), "p/", "/", "", 1000)
	if err != nil {
		t.Fatalf("ListObjectsDelimited: %v", err)
	}
	if got, want := res.CommonPrefixes, []string{"p/a/", "p/b/"}; !slices.Equal(got, want) {
		t.Errorf("CommonPrefixes = %v, want %v", got, want)
	}
	if len(res.Objects) != 2 || res.Objects[0].ObjectKey != "p/file1.txt" || res.Objects[1].ObjectKey != "p/file2.txt" {
		t.Errorf("Objects = %+v, want p/file1.txt,p/file2.txt", res.Objects)
	}
	if res.Objects[0].BackendName != "backend-a" || res.Objects[0].SizeBytes != 10 {
		t.Errorf("leaf location not populated: %+v", res.Objects[0])
	}
	if res.IsTruncated {
		t.Error("IsTruncated = true, want false")
	}
}

// TestListObjectsDelimited_DeepKeysCollapseAtFirstDelimiter verifies a deeply
// nested key collapses to the first-level CommonPrefix only.
func TestListObjectsDelimited_DeepKeysCollapseAtFirstDelimiter(t *testing.T) {
	s := newTestStore(t)
	for _, k := range []string{"p/a/b/c/d.txt", "p/a/x.txt", "p/a/y/z.txt"} {
		mustRecordObject(t, s, k, "backend-a", 1)
	}
	cps, leaves := leafKeys(t, s, "p/", "/", "", 1000)
	if !slices.Equal(cps, []string{"p/a/"}) {
		t.Errorf("CommonPrefixes = %v, want [p/a/]", cps)
	}
	if len(leaves) != 0 {
		t.Errorf("leaves = %v, want none", leaves)
	}
}

// TestListObjectsDelimited_SkipsLargeGroup verifies a single large group
// collapses to one CommonPrefix (the loose scan seeks past it).
func TestListObjectsDelimited_SkipsLargeGroup(t *testing.T) {
	s := newTestStore(t)
	for i := range 50 {
		mustRecordObject(t, s, fmt.Sprintf("p/big/k%04d.txt", i), "backend-a", 1)
	}
	mustRecordObject(t, s, "p/zzz.txt", "backend-a", 1)

	cps, leaves := leafKeys(t, s, "p/", "/", "", 5)
	if !slices.Equal(cps, []string{"p/big/"}) {
		t.Errorf("CommonPrefixes = %v, want [p/big/]", cps)
	}
	if !slices.Equal(leaves, []string{"p/zzz.txt"}) {
		t.Errorf("leaves = %v, want [p/zzz.txt]", leaves)
	}
}

// TestListObjectsDelimited_PaginationNoDuplicate verifies paginating across
// calls via the continuation token emits every CommonPrefix exactly once.
func TestListObjectsDelimited_PaginationNoDuplicate(t *testing.T) {
	s := newTestStore(t)
	for g := range 4 {
		for k := range 3 {
			mustRecordObject(t, s, fmt.Sprintf("p/g%d/k%d.txt", g, k), "backend-a", 1)
		}
	}

	seen := map[string]int{}
	after := ""
	for range 11 {
		res, err := s.ListObjectsDelimited(context.Background(), "p/", "/", after, 2)
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
		cp := fmt.Sprintf("p/g%d/", g)
		if seen[cp] != 1 {
			t.Errorf("CommonPrefix %q emitted %d times, want 1", cp, seen[cp])
		}
	}
	if len(seen) != 4 {
		t.Errorf("distinct prefixes = %d, want 4: %v", len(seen), seen)
	}
}
