// -------------------------------------------------------------------------------
// Core Listing - Page Trimming Tests
//
// Author: Alex Freidah
//
// Pure-function coverage for ListPage, TrimPage, the key range helpers and the
// page builder both engines share, so the routing, truncation and
// continuation-token contract holds independently of the engine that fetched
// the rows.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// TestTrimPage verifies rows fetched one past the limit are cut back to it and
// reported truncated, while a short or exact fetch passes through untruncated.
func TestTrimPage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		rows          []int
		limit         int
		wantPage      []int
		wantTruncated bool
	}{
		{"extra row", []int{1, 2, 3}, 2, []int{1, 2}, true},
		{"exact", []int{1, 2}, 2, []int{1, 2}, false},
		{"short", []int{1}, 2, []int{1}, false},
		{"zero limit with rows", []int{1}, 0, []int{}, true},
		{"empty", nil, 2, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			page, truncated := TrimPage(tc.rows, tc.limit)
			if !slices.Equal(page, tc.wantPage) || truncated != tc.wantTruncated {
				t.Errorf("TrimPage(%v, %d) = %v, %v; want %v, %v", tc.rows, tc.limit, page, truncated, tc.wantPage, tc.wantTruncated)
			}
		})
	}
}

// TestListPage_RoutesByDelimiterAndAnswersZero verifies a delimiter selects
// the delimited query, an empty one the flat query, and a zero page size
// answers an empty page without calling either.
func TestListPage_RoutesByDelimiterAndAnswersZero(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, delimiter string
		maxKeys         int
		want            string
	}{
		{"flat", "", 5, "flat"},
		{"delimited", "/", 5, "delimited"},
		{"zero", "/", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			called := ""
			flat := func(context.Context, string, string, int) ([]ObjectLocation, error) {
				called = "flat"
				return nil, nil
			}
			delimited := func(context.Context, string, string, string, string, int) ([]ObjectLocation, error) {
				called = "delimited"
				return nil, nil
			}
			res, err := ListPage(t.Context(), "p/", tc.delimiter, "", tc.maxKeys, flat, delimited)
			if err != nil || res == nil {
				t.Fatalf("ListPage = %v, %v; want a page", res, err)
			}
			if called != tc.want {
				t.Errorf("called %q, want %q", called, tc.want)
			}
		})
	}
}

// TestListPage_RangeTheReaderGets verifies the bounds handed to each reader: the
// prefix range, a cursor stepping just past a key, and a cursor naming a group
// stepping past the whole group while one inside it stays inside.
func TestListPage_RangeTheReaderGets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, delimiter, after, wantFrom string
	}{
		{"first page", "", "", "p/"},
		{"flat cursor", "", "p/a", "p/a\x01"},
		{"cursor below the prefix", "", "a", "p/"},
		{"cursor on a group", "/", "p/b/", "p/b0"},
		{"cursor inside a group", "/", "p/b/x", "p/b/x\x01"},
		{"flat cursor shaped like a group", "", "p/b/", "p/b/\x01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var from, end string
			var limit int
			read := func(_ context.Context, f, e string, l int) ([]ObjectLocation, error) {
				from, end, limit = f, e, l
				return nil, nil
			}
			delimited := func(ctx context.Context, _, _, f, e string, l int) ([]ObjectLocation, error) {
				return read(ctx, f, e, l)
			}
			if _, err := ListPage(t.Context(), "p/", tc.delimiter, tc.after, 5, read, delimited); err != nil {
				t.Fatalf("ListPage: %v", err)
			}
			if from != tc.wantFrom || end != "p0" || limit != 6 {
				t.Errorf("reader got from %q, end %q, limit %d; want %q, p0, 6", from, end, limit, tc.wantFrom)
			}
		})
	}
}

// TestListPage_ReaderErrorSurfaces verifies a failed read returns the error
// rather than an empty page.
func TestListPage_ReaderErrorSurfaces(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	read := func(context.Context, string, string, int) ([]ObjectLocation, error) { return nil, boom }
	if _, err := ListPage(t.Context(), "p/", "", "", 5, read, nil); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}

// TestBuildPage_TokenIsLastEntry verifies a truncated page resumes from its last
// entry, a key or a group name, and that an untruncated page carries no token.
func TestBuildPage_TokenIsLastEntry(t *testing.T) {
	t.Parallel()
	rows := func() []ObjectLocation {
		return []ObjectLocation{{ObjectKey: "p/a"}, {ObjectKey: "p/b/x"}, {ObjectKey: "p/c"}}
	}

	page := buildPage(rows(), "p/", "", 2)
	if page.KeyCount() != 2 || !page.IsTruncated || page.NextContinuationToken != "p/b/x" {
		t.Errorf("flat page = %+v, want 2 keys truncated at p/b/x", page)
	}
	page = buildPage(rows(), "p/", "/", 2)
	if page.KeyCount() != 2 || !page.IsTruncated || page.NextContinuationToken != "p/b/" {
		t.Errorf("delimited page = %+v, want 2 entries truncated at the group", page)
	}
	if !slices.Equal(page.CommonPrefixes, []string{"p/b/"}) || len(page.Objects) != 1 {
		t.Errorf("split = %v prefixes / %d objects, want [p/b/] / 1", page.CommonPrefixes, len(page.Objects))
	}
	page = buildPage(rows(), "p/", "/", 3)
	if page.KeyCount() != 3 || page.IsTruncated || page.NextContinuationToken != "" {
		t.Errorf("full page = %+v, want 3 entries untruncated", page)
	}
}

// TestCommonPrefix verifies a key folds at the first delimiter after the prefix,
// a multi-byte delimiter included, and that nothing else folds.
func TestCommonPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, prefix, delimiter, key, want string
		wantOK                             bool
	}{
		{"first delimiter", "p/", "/", "p/a/b/c", "p/a/", true},
		{"key is the group", "p/", "/", "p/a/", "p/a/", true},
		{"leaf", "p/", "/", "p/a", "", false},
		{"key equals the prefix", "p/", "/", "p/", "", false},
		{"outside the prefix", "p/", "/", "q/a/", "", false},
		{"long delimiter", "p/", "--", "p/a--b", "p/a--", true},
		{"no delimiter", "p/", "", "p/a/", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := commonPrefix(tc.prefix, tc.delimiter, tc.key)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("commonPrefix = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestPrefixEnd verifies the bound sits above every key under the prefix and no
// other, across the code points where raising a character is not a simple add.
func TestPrefixEnd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, prefix, want string
	}{
		{"ascii", "a/", "a0"},
		{"multi-byte", "caf\U000000E9", "caf\U000000EA"},
		{"continuation byte rolls over", "\U000000BF", "\U000000C0"},
		{"before the surrogates", "a\U0000D7FF", "a\U0000E000"},
		{"highest code point rolls back", "a\U0010FFFF", "b"},
		{"nothing can rise", "\U0010FFFF", "\U0010FFFF"},
		{"empty", "", "\U0010FFFF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PrefixEnd(tc.prefix); got != tc.want {
				t.Errorf("PrefixEnd(%q) = %q, want %q", tc.prefix, got, tc.want)
			}
		})
	}
}

// TestKeysFrom verifies the scan starts at the prefix until the cursor passes it,
// and then just past the cursor.
func TestKeysFrom(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, prefix, after, want string
	}{
		{"no cursor", "p/", "", "p/"},
		{"cursor below the prefix", "p/", "a", "p/"},
		{"cursor equals the prefix", "p/", "p/", "p/\x01"},
		{"cursor under the prefix", "p/", "p/a", "p/a\x01"},
		{"no prefix", "", "a", "a\x01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := KeysFrom(tc.prefix, tc.after); got != tc.want {
				t.Errorf("KeysFrom(%q, %q) = %q, want %q", tc.prefix, tc.after, got, tc.want)
			}
		})
	}
}

// TestBuildDirectoryListing_TrimsFileDetail verifies the file fetched past
// maxKeys is left out of the entries and the cursor is the last kept file,
// while a directory roll-up is listed regardless of the file page.
func TestBuildDirectoryListing_TrimsFileDetail(t *testing.T) {
	t.Parallel()
	stats := []DirStat{{Name: "a"}, {Name: "b"}, {Name: "sub/", IsDir: true, FileCount: 4}}
	files := []DirFile{{ObjectKey: "p/a", SizeBytes: 1}, {ObjectKey: "p/b", SizeBytes: 2}}

	res := BuildDirectoryListing("p/", stats, files, 1)
	if !res.HasMore || res.NextCursor != "p/a" {
		t.Errorf("HasMore = %v NextCursor = %q, want true / p/a", res.HasMore, res.NextCursor)
	}
	names := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"p/a", "p/sub/"}) {
		t.Errorf("entries = %v, want [p/a p/sub/]", names)
	}
}
