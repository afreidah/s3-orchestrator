// -------------------------------------------------------------------------------
// Prefix Listing Check
//
// Author: Alex Freidah
//
// Every prefix read states a byte range, and each engine writes its own SQL for
// it. The check runs against both, so what a prefix matches, how keys group and
// how a page resumes are held to one behavior.
// -------------------------------------------------------------------------------

package storetest

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// listedSize is the size every object in the check is recorded with.
const listedSize = 10

// wildcardPrefix is a prefix whose underscore must match only itself.
const wildcardPrefix = "my_b/"

// ListingStore is the slice of a store CheckListing drives.
type ListingStore interface {
	RecordObject(ctx context.Context, req *core.RecordObjectRequest) ([]core.CleanupItem, error)
	DeleteObject(ctx context.Context, key string) ([]core.CleanupItem, error)
	ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*core.ListObjectsResult, error)
	CountObjectsByPrefix(ctx context.Context, prefix string) (int64, error)
	ListExpiredObjects(ctx context.Context, q core.ExpiredObjectsQuery) ([]core.ObjectLocation, error)
	ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error)
	CreateMultipartUpload(ctx context.Context, params *core.CreateMultipartUploadParams) (bool, error)
	DeleteMultipartUpload(ctx context.Context, uploadID, reason string, staleBefore time.Time) ([]core.CleanupItem, error)
	ListMultipartUploads(ctx context.Context, prefix string, maxUploads int) ([]core.MultipartUpload, error)
	CountActiveMultipartUploads(ctx context.Context, bucketPrefix string) (int64, error)
}

// CheckListing runs the prefix reads over keys under root, which nothing else
// may write under, each key held on both backends so a read must also fold
// copies. Every listing is paged at several sizes, because a page boundary is
// where a key can be lost or repeated.
func CheckListing(t *testing.T, s ListingStore, root, backend, replica string) {
	t.Helper()
	l := lister{s: s, root: root, backend: backend, replica: replica}
	for _, tc := range listingCases() {
		t.Run(tc.name, func(t *testing.T) { l.checkPages(t, &tc) })
	}
	t.Run("leaf columns", l.checkLeafColumns)
	t.Run("count", l.checkCount)
	t.Run("expired", l.checkExpired)
	t.Run("directory", l.checkDirectory)
	t.Run("multipart", l.checkMultipart)
}

// -------------------------------------------------------------------------
// LISTING CASES
// -------------------------------------------------------------------------

// listingCase is one listing, with its keys, prefix, cursor and entries written
// relative to a corner of the keyspace the case has to itself.
type listingCase struct {
	name, prefix, delimiter, after string
	keys, want                     []string
}

// listingCases returns the listings every engine must answer alike.
func listingCases() []listingCase {
	const cafe = "caf\U000000E9/"
	wide := []string{"zzz"}
	for i := range 50 {
		wide = append(wide, fmt.Sprintf("big/k%04d", i))
	}
	group := []string{"g/1", "g/2", "h"}
	underPrefix := []string{"p/a", "p/b"}
	return []listingCase{
		{name: "flat", keys: []string{"a/1", "a/2", "b"}, want: []string{"a/1", "a/2", "b"}},
		{name: "groups and leaves", delimiter: "/", keys: []string{"a/1", "a/2", "b/1", "f1", "f2"}, want: []string{"a/", "b/", "f1", "f2"}},
		{name: "folds at the first delimiter", delimiter: "/", keys: []string{"a/b/c/d", "a/x", "a/y/z"}, want: []string{"a/"}},
		{name: "long delimiter", delimiter: "--", keys: []string{"a--1", "a--2", "b"}, want: []string{"a--", "b"}},
		{name: "a group wider than the page", delimiter: "/", keys: wide, want: []string{"big/", "zzz"}},
		{name: "a key equal to the bound past a group", delimiter: "/", keys: []string{"logs/a", "logs/b", "logs0", "logs1"}, want: []string{"logs/", "logs0", "logs1"}},
		{name: "a key equal to the prefix", prefix: "p/", keys: []string{"p/", "p/a", "p0"}, want: []string{"p/", "p/a"}},
		{name: "underscore matches only itself", prefix: wildcardPrefix, keys: wildcardKeys(""), want: wildcardKeys("")[:2]},
		{name: "percent matches only itself", prefix: "my%b/", keys: []string{"my%b/a", "myXXb/a"}, want: []string{"my%b/a"}},
		{name: "non-ascii prefix", prefix: cafe, delimiter: "/", keys: []string{cafe + "a", cafe + "d/1", "caf\U000000EA/a"}, want: []string{cafe + "a", cafe + "d/"}},
		{name: "the highest code point stays under its prefix", prefix: "d/", keys: []string{"d/\U0010FFFF", "d0"}, want: []string{"d/\U0010FFFF"}},
		{name: "cursor on a key", after: "a", keys: []string{"a", "b", "c"}, want: []string{"b", "c"}},
		{name: "cursor below the prefix", prefix: "p/", after: "a", keys: underPrefix, want: underPrefix},
		{name: "cursor past the prefix", prefix: "p/", after: "q", keys: underPrefix},
		{name: "cursor on a group", delimiter: "/", after: "g/", keys: group, want: []string{"h"}},
		{name: "cursor inside a group", delimiter: "/", after: "g/1", keys: group, want: []string{"g/", "h"}},
	}
}

// wildcardKeys returns two keys under wildcardPrefix, then one the underscore
// must not reach, all under base.
func wildcardKeys(base string) []string {
	return []string{base + wildcardPrefix + "a", base + wildcardPrefix + "b", base + "myXb/a"}
}

// -------------------------------------------------------------------------
// CHECKS
// -------------------------------------------------------------------------

// lister carries the store and where the check writes.
type lister struct {
	s                      ListingStore
	root, backend, replica string
}

// checkPages asserts a case's entries come back in order and exactly once at
// every page size.
func (l lister) checkPages(t *testing.T, tc *listingCase) {
	base := l.root + tc.name + "/"
	l.seed(t, prefixed(base, tc.keys)...)
	after := ""
	if tc.after != "" {
		after = base + tc.after
	}
	want := prefixed(base, tc.want)
	for _, size := range []int{1, 2, 100} {
		if got := l.pageThrough(t, base+tc.prefix, tc.delimiter, after, size); !slices.Equal(got, want) {
			t.Errorf("page size %d: entries = %q, want %q", size, got, want)
		}
	}
}

// checkLeafColumns asserts a listed leaf reports the backend it read and the
// object's size, on both listing paths.
func (l lister) checkLeafColumns(t *testing.T) {
	base := l.root + "leaf/"
	l.seed(t, base+"a")
	for _, delimiter := range []string{"", "/"} {
		page, err := l.s.ListObjects(context.Background(), base, delimiter, "", 10)
		if err != nil {
			t.Fatalf("ListObjects: %v", err)
		}
		if len(page.Objects) != 1 {
			t.Fatalf("delimiter %q: objects = %+v, want one", delimiter, page.Objects)
		}
		if got := page.Objects[0]; got.BackendName == "" || got.SizeBytes != listedSize {
			t.Errorf("delimiter %q: leaf = %+v, want its backend and size %d", delimiter, got, listedSize)
		}
	}
}

// checkCount asserts a count reports keys rather than copies, stays inside its
// prefix, and answers zero rather than failing for an empty one.
func (l lister) checkCount(t *testing.T) {
	base := l.root + "count/"
	l.seed(t, wildcardKeys(base)...)
	for prefix, want := range map[string]int64{wildcardPrefix: 2, "empty/": 0} {
		got, err := l.s.CountObjectsByPrefix(context.Background(), base+prefix)
		if err != nil {
			t.Fatalf("CountObjectsByPrefix: %v", err)
		}
		if got != want {
			t.Errorf("count of %s = %d, want %d", prefix, got, want)
		}
	}
}

// checkExpired asserts an expiry page stays inside its prefix, resumes past its
// cursor, and stops at its limit.
func (l lister) checkExpired(t *testing.T) {
	base := l.root + "expired/"
	keys := wildcardKeys(base)
	l.seed(t, keys...)
	expired := func(after string, limit int) []string {
		t.Helper()
		rows, err := l.s.ListExpiredObjects(context.Background(), core.ExpiredObjectsQuery{
			Prefix: base + wildcardPrefix, After: after, Cutoff: time.Now().Add(time.Hour), Limit: limit,
		})
		if err != nil {
			t.Fatalf("ListExpiredObjects: %v", err)
		}
		return core.KeysOf(rows)
	}
	if got := expired("", 10); !slices.Equal(got, keys[:2]) {
		t.Errorf("expired = %q, want both keys under the prefix", got)
	}
	if got := expired("", 1); !slices.Equal(got, keys[:1]) {
		t.Errorf("limited = %q, want the first key", got)
	}
	if got := expired(keys[0], 10); !slices.Equal(got, keys[1:2]) {
		t.Errorf("after the cursor = %q, want the second key", got)
	}
}

// checkDirectory asserts the dashboard's view of a directory whose prefix a
// wildcard or a multi-byte character could shift: one-character names survive,
// files sort by byte, and the file page resumes past its cursor.
func (l lister) checkDirectory(t *testing.T) {
	for _, dir := range []string{"d_r/", "d\U000000EFr/"} {
		base := l.root + "directory/" + dir
		l.seed(t, base+"B", base+"a", base+"sub/x")
		children := func(after string, maxKeys int) *core.DirectoryListResult {
			t.Helper()
			page, err := l.s.ListDirectoryChildren(context.Background(), base, after, maxKeys)
			if err != nil {
				t.Fatalf("ListDirectoryChildren: %v", err)
			}
			return page
		}

		if got, want := entryNames(children("", 100)), prefixed(base, []string{"sub/", "B", "a"}); !slices.Equal(got, want) {
			t.Errorf("entries = %q, want %q", got, want)
		}
		first := children("", 1)
		if !first.HasMore || first.NextCursor != base+"B" {
			t.Fatalf("first page = %+v, want more after %sB", first, base)
		}
		if got, want := entryNames(children(first.NextCursor, 1)), prefixed(base, []string{"sub/", "a"}); !slices.Equal(got, want) {
			t.Errorf("next page = %q, want %q", got, want)
		}
	}
}

// checkMultipart asserts in-progress uploads are listed and counted inside their
// prefix.
func (l lister) checkMultipart(t *testing.T) {
	base := l.root + "multipart/"
	ctx := context.Background()
	keys := wildcardKeys(base)
	for _, key := range keys {
		created, err := l.s.CreateMultipartUpload(ctx, &core.CreateMultipartUploadParams{
			UploadID: key, ObjectKey: key, BackendName: l.backend,
		})
		if err != nil || !created {
			t.Fatalf("CreateMultipartUpload(%s) = %v, %v", key, created, err)
		}
		t.Cleanup(func() {
			_, _ = l.s.DeleteMultipartUpload(context.Background(), key, core.CleanupReasonDeleted, time.Now())
		})
	}
	uploads, err := l.s.ListMultipartUploads(ctx, base+wildcardPrefix, 10)
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if len(uploads) != 2 || uploads[0].ObjectKey != keys[0] || uploads[1].ObjectKey != keys[1] {
		t.Errorf("uploads = %+v, want the two under the prefix in key order", uploads)
	}
	count, err := l.s.CountActiveMultipartUploads(ctx, base+wildcardPrefix)
	if err != nil {
		t.Fatalf("CountActiveMultipartUploads: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// seed records each key on both backends and removes it when the test ends.
func (l lister) seed(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		_, err := l.s.RecordObject(context.Background(), &core.RecordObjectRequest{
			Key: key, Size: listedSize,
			Copies: []core.ObjectCopy{{Backend: l.backend}, {Backend: l.replica}},
		})
		if err != nil {
			t.Fatalf("RecordObject(%s): %v", key, err)
		}
		t.Cleanup(func() { _, _ = l.s.DeleteObject(context.Background(), key) })
	}
}

// pageThrough lists a prefix to its end and returns each page's entries in key
// order. A page holds groups and leaves apart, so they are merged per page.
func (l lister) pageThrough(t *testing.T, prefix, delimiter, after string, size int) []string {
	t.Helper()
	got := []string{}
	for range 100 {
		page, err := l.s.ListObjects(context.Background(), prefix, delimiter, after, size)
		if err != nil {
			t.Fatalf("ListObjects: %v", err)
		}
		entries := slices.Concat(page.CommonPrefixes, core.KeysOf(page.Objects))
		slices.Sort(entries)
		got = append(got, entries...)
		if !page.IsTruncated {
			return got
		}
		after = page.NextContinuationToken
	}
	t.Fatalf("paging %q at size %d did not end", prefix, size)
	return nil
}

// prefixed returns names with base in front of each.
func prefixed(base string, names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = base + name
	}
	return out
}

// entryNames returns a directory page's entry names in the order it gave them.
func entryNames(page *core.DirectoryListResult) []string {
	names := make([]string, len(page.Entries))
	for i := range page.Entries {
		names[i] = page.Entries[i].Name
	}
	return names
}
