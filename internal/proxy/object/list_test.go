// -------------------------------------------------------------------------------
// ListObjects Tests - Delimiter Grouping, Group Skipping, Pagination
//
// Author: Alex Freidah
//
// Unit coverage for Manager.ListObjects: virtual-directory CommonPrefix
// grouping with interleaved leaf objects, the cursor-skip that keeps per-call
// cost proportional to emitted prefixes rather than scanned keys, and
// duplicate-free CommonPrefix emission across paginated calls. Cross-engine
// behavior parity is covered by the integration suite; these pin the
// proxy-side grouping and skip logic.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// fakeListStore serves a sorted keyspace from memory, filtering by prefix and
// startAfter and capping at maxKeys with a truncation flag, mirroring the real
// store contract. It counts pages served so tests can assert the manager does
// not walk a whole group page by page.
type fakeListStore struct {
	ObjectStores // embedded nil; only ListObjects is implemented

	keys  []string
	pages int
}

// newFakeListStore sorts keys once so ListObjects can serve ordered pages.
func newFakeListStore(keys []string) *fakeListStore {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	return &fakeListStore{keys: sorted}
}

// ListObjects returns the keys under prefix that sort after startAfter, capped
// at maxKeys, flagging truncation when more match.
func (s *fakeListStore) ListObjects(_ context.Context, prefix, startAfter string, maxKeys int) (*core.ListObjectsResult, error) {
	s.pages++

	matched := make([]string, 0, maxKeys+1)
	for _, k := range s.keys {
		if strings.HasPrefix(k, prefix) && k > startAfter {
			matched = append(matched, k)
		}
	}

	truncated := len(matched) > maxKeys
	if truncated {
		matched = matched[:maxKeys]
	}
	objs := make([]core.ObjectLocation, len(matched))
	for i, k := range matched {
		objs[i] = core.ObjectLocation{ObjectKey: k}
	}
	return &core.ListObjectsResult{Objects: objs, IsTruncated: truncated}, nil
}

// objectKeys extracts the object keys from a result for comparison.
func objectKeys(res *ListObjectsV2Result) []string {
	out := make([]string, len(res.Objects))
	for i := range res.Objects {
		out[i] = res.Objects[i].ObjectKey
	}
	return out
}

// TestListObjects_DelimiterGroupsAndLeaves verifies a single-page delimiter
// list folds keys with a delimiter after the prefix into CommonPrefixes while
// returning keys without one as plain objects.
func TestListObjects_DelimiterGroupsAndLeaves(t *testing.T) {
	defer quietLogs()()
	store := newFakeListStore([]string{
		"p/a/1.txt", "p/a/2.txt", // -> CommonPrefix p/a/
		"p/b/1.txt",      // -> CommonPrefix p/b/
		"p/file1.txt",    // -> leaf object
		"p/file2.txt",    // -> leaf object
		"other/skip.txt", // outside the prefix
	})
	m := newBenchListManager(store)

	res, err := m.ListObjects(context.Background(), "p/", "/", "", 1000)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}

	wantCP := []string{"p/a/", "p/b/"}
	if !equalStrings(res.CommonPrefixes, wantCP) {
		t.Errorf("CommonPrefixes = %v, want %v", res.CommonPrefixes, wantCP)
	}
	wantObjs := []string{"p/file1.txt", "p/file2.txt"}
	if got := objectKeys(res); !equalStrings(got, wantObjs) {
		t.Errorf("Objects = %v, want %v", got, wantObjs)
	}
	if res.KeyCount != 4 {
		t.Errorf("KeyCount = %d, want 4", res.KeyCount)
	}
	if res.IsTruncated {
		t.Error("IsTruncated = true, want false")
	}
}

// TestListObjects_DelimiterSkipsLargeGroupBoundedPages is the regression guard
// for the cursor skip: a single large group spanning many store pages must be
// collapsed without paging through every key. Before the fix this walked the
// group page by page (group_size/maxKeys pages); after it, one page discovers
// the group and the next seek skips past it.
func TestListObjects_DelimiterSkipsLargeGroupBoundedPages(t *testing.T) {
	defer quietLogs()()
	keys := make([]string, 0, 51)
	for i := range 50 {
		keys = append(keys, fmt.Sprintf("p/big/k%04d.txt", i))
	}
	keys = append(keys, "p/zzz/1.txt")
	store := newFakeListStore(keys)
	m := newBenchListManager(store)

	// maxKeys=5 means each store page holds at most 5 rows; the "big" group is
	// 50 keys, so the unfixed loop would need ~10 pages just to clear it.
	res, err := m.ListObjects(context.Background(), "p/", "/", "", 5)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}

	wantCP := []string{"p/big/", "p/zzz/"}
	if !equalStrings(res.CommonPrefixes, wantCP) {
		t.Errorf("CommonPrefixes = %v, want %v", res.CommonPrefixes, wantCP)
	}
	// Two groups: one page discovers each, plus a terminal empty page. Without
	// the skip this would be ~11.
	if store.pages > 3 {
		t.Errorf("store pages = %d, want <= 3 (group should be skipped, not walked)", store.pages)
	}
}

// TestListObjects_DelimiterPaginationNoDuplicate verifies that paginating a
// delimiter list across calls emits every CommonPrefix exactly once with no
// gaps, using the returned continuation token as the next startAfter.
func TestListObjects_DelimiterPaginationNoDuplicate(t *testing.T) {
	defer quietLogs()()
	var keys []string
	for g := range 4 {
		for k := range 3 {
			keys = append(keys, fmt.Sprintf("p/g%d/k%d.txt", g, k))
		}
	}
	store := newFakeListStore(keys)
	m := newBenchListManager(store)

	seen := map[string]int{}
	cursor := ""
	for calls := 0; ; calls++ {
		if calls > 10 {
			t.Fatal("pagination did not terminate")
		}
		res, err := m.ListObjects(context.Background(), "p/", "/", cursor, 2)
		if err != nil {
			t.Fatalf("ListObjects: %v", err)
		}
		for _, cp := range res.CommonPrefixes {
			seen[cp]++
		}
		if !res.IsTruncated {
			break
		}
		cursor = res.NextContinuationToken
	}

	for g := range 4 {
		cp := fmt.Sprintf("p/g%d/", g)
		if seen[cp] != 1 {
			t.Errorf("CommonPrefix %q emitted %d times, want exactly 1", cp, seen[cp])
		}
	}
	if len(seen) != 4 {
		t.Errorf("emitted %d distinct prefixes, want 4: %v", len(seen), seen)
	}
}

// TestListObjects_NoDelimiterReturnsObjects is the control: without a
// delimiter every key is returned as a plain object, capped at maxKeys.
func TestListObjects_NoDelimiterReturnsObjects(t *testing.T) {
	defer quietLogs()()
	store := newFakeListStore([]string{
		"p/a/1.txt", "p/a/2.txt", "p/b/1.txt", "p/file.txt",
	})
	m := newBenchListManager(store)

	res, err := m.ListObjects(context.Background(), "p/", "", "", 1000)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}

	if len(res.CommonPrefixes) != 0 {
		t.Errorf("CommonPrefixes = %v, want none", res.CommonPrefixes)
	}
	want := []string{"p/a/1.txt", "p/a/2.txt", "p/b/1.txt", "p/file.txt"}
	if got := objectKeys(res); !equalStrings(got, want) {
		t.Errorf("Objects = %v, want %v", got, want)
	}
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
