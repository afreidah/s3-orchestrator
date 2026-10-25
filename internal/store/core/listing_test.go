// -------------------------------------------------------------------------------
// Core Listing - Page Trimming Tests
//
// Author: Alex Freidah
//
// Pure-function coverage for ListPage, TrimPage and the flat and delimited page
// builders both engines share, so the routing, truncation and continuation-token
// contract holds independently of the engine that fetched the rows.
// -------------------------------------------------------------------------------

package core

import (
	"context"
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
			flat := func(context.Context, string, string, int) (*ListObjectsResult, error) {
				called = "flat"
				return &ListObjectsResult{}, nil
			}
			delimited := func(context.Context, string, string, string, int) (*ListObjectsResult, error) {
				called = "delimited"
				return &ListObjectsResult{}, nil
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

// TestBuildListPage_TokenIsLastKeptKey verifies a truncated flat page resumes
// from the last key it kept, and an untruncated one carries no token.
func TestBuildListPage_TokenIsLastKeptKey(t *testing.T) {
	t.Parallel()
	objects := []ObjectLocation{{ObjectKey: "a"}, {ObjectKey: "b"}, {ObjectKey: "c"}}

	page := BuildListPage(objects, 2)
	if page.KeyCount() != 2 || !page.IsTruncated || page.NextContinuationToken != "b" {
		t.Errorf("BuildListPage(3 rows, 2) = %+v, want 2 keys truncated at b", page)
	}
	page = BuildListPage(objects, 3)
	if page.KeyCount() != 3 || page.IsTruncated || page.NextContinuationToken != "" {
		t.Errorf("BuildListPage(3 rows, 3) = %+v, want 3 keys untruncated", page)
	}
}

// TestBuildDelimitedPage_TokenIsLastSkipBound verifies a truncated delimited
// page resumes from the skip bound of the last entry it kept, so the next page
// starts past that whole group, and that KeyCount counts a prefix as one.
func TestBuildDelimitedPage_TokenIsLastSkipBound(t *testing.T) {
	t.Parallel()
	entries := []DelimitedEntry{
		{Leaf: ObjectLocation{ObjectKey: "p/a"}, SkipBound: "p/a"},
		{IsPrefix: true, CommonPrefix: "p/b/", SkipBound: "p/b0"},
		{Leaf: ObjectLocation{ObjectKey: "p/c"}, SkipBound: "p/c"},
	}

	page := BuildDelimitedPage(entries, 2)
	if page.KeyCount() != 2 || !page.IsTruncated || page.NextContinuationToken != "p/b0" {
		t.Errorf("BuildDelimitedPage(3 entries, 2) = %+v, want 2 entries truncated at p/b0", page)
	}
	if !slices.Equal(page.CommonPrefixes, []string{"p/b/"}) || len(page.Objects) != 1 {
		t.Errorf("BuildDelimitedPage split = %v prefixes / %d objects, want [p/b/] / 1", page.CommonPrefixes, len(page.Objects))
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
