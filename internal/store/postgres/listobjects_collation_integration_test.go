// -------------------------------------------------------------------------------
// ListObjects Collation - Integration Tests
//
// Author: Alex Freidah
//
// S3 ListObjectsV2 returns keys in UTF-8 byte order. object_key is plain TEXT,
// so without COLLATE "C" it sorts under the database's LC_COLLATE instead, and
// the same request answers differently on Postgres than on SQLite. These tests
// seed the collation-sensitive key set from the reconcile cursor tests, then
// assert the listing pages in byte order, that no key is skipped or repeated
// across pages, and that the delimited listing agrees with the flat one. An ICU
// control proves the key set actually reorders under a locale collation, so the
// assertions still mean something on a container whose default already sorts by
// byte.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"slices"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// seedCollationKeys records the adversarial key set under a caller-supplied
// prefix and returns the prefixed keys. Every test in this package shares one
// database, so an unprefixed listing would sweep up other tests' objects; a
// constant prefix keeps the listing scoped without disturbing relative order,
// which is what these tests assert on.
func seedCollationKeys(t *testing.T, s *Store, backendName, prefix string) []string {
	t.Helper()
	ctx := context.Background()
	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: backendName, QuotaBytes: 1 << 30},
	}); err != nil {
		t.Fatalf("SyncQuotaLimits(%q): %v", backendName, err)
	}
	keys := make([]string, 0, len(adversarialKeys))
	for _, k := range adversarialKeys {
		full := prefix + k
		if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: full, Backend: backendName, Size: 1}); err != nil {
			t.Fatalf("RecordObject(%q): %v", full, err)
		}
		keys = append(keys, full)
	}
	return keys
}

// pageAllObjects walks ListObjects with a small page size, returning every key
// in the order the listing produced it. The page size is deliberately smaller
// than the key set so the cursor predicate is exercised rather than a single
// unpaged read.
func pageAllObjects(t *testing.T, s *Store, prefix string, pageSize int) []string {
	t.Helper()
	ctx := context.Background()
	var got []string
	cursor := ""
	for {
		res, err := s.ListObjects(ctx, prefix, cursor, pageSize)
		if err != nil {
			t.Fatalf("ListObjects(startAfter=%q): %v", cursor, err)
		}
		if len(res.Objects) == 0 {
			break
		}
		for i := range res.Objects {
			got = append(got, res.Objects[i].ObjectKey)
		}
		cursor = res.Objects[len(res.Objects)-1].ObjectKey
		if !res.IsTruncated {
			break
		}
	}
	return got
}

// icuOrder reads the same keys back under an explicit ICU locale collation, the
// control that proves the seeded set is collation-sensitive.
func icuOrder(t *testing.T, s *Store, prefix string) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT object_key FROM object_locations
		 WHERE object_key LIKE $1 || '%'
		 ORDER BY object_key COLLATE "en-US-x-icu"`, prefix)
	if err != nil {
		t.Fatalf("ICU control query (is the en-US-x-icu collation available?): %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan ICU row: %v", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ICU control rows: %v", err)
	}
	return out
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStoreInt_ListObjects_ByteOrder asserts the flat listing answers in UTF-8
// byte order, which is what S3 ListObjectsV2 specifies and what SQLite's BINARY
// collation already produces.
func TestStoreInt_ListObjects_ByteOrder(t *testing.T) {
	s := adapterPgStore(t)
	const prefix = "collate-order/"
	keys := seedCollationKeys(t, s, "listobjects-collate-be", prefix)

	var datcollate string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT datcollate FROM pg_database WHERE datname = current_database()`,
	).Scan(&datcollate); err != nil {
		t.Fatalf("read datcollate: %v", err)
	}
	t.Logf("database datcollate = %q", datcollate)

	got := pageAllObjects(t, s, prefix, 3)

	want := slices.Clone(keys)
	slices.Sort(want) // Go string sort is byte order.
	if !slices.Equal(got, want) {
		t.Fatalf("listing order mismatch:\n got  %q\n want %q", got, want)
	}

	// Without this control a container whose default collation already byte-orders
	// would pass the assertion above no matter what the query said.
	if icu := icuOrder(t, s, prefix); slices.Equal(icu, want) {
		t.Fatalf("ICU order equals byte order; key set is not collation-sensitive "+
			"(or ICU collation unavailable). icu=%q byte=%q", icu, want)
	}
}

// TestStoreInt_ListObjects_PaginationCoversEveryKeyOnce asserts paging neither
// skips nor repeats. The cursor predicate and the ORDER BY have to share a
// collation for this to hold; splitting them pages a byte-ordered scan with a
// locale-ordered cursor, which drops keys silently.
func TestStoreInt_ListObjects_PaginationCoversEveryKeyOnce(t *testing.T) {
	s := adapterPgStore(t)
	const prefix = "collate-page/"
	keys := seedCollationKeys(t, s, "listobjects-collate-page-be", prefix)

	seen := map[string]int{}
	for _, k := range pageAllObjects(t, s, prefix, 2) {
		seen[k]++
	}

	for _, k := range keys {
		switch seen[k] {
		case 1: // covered exactly once, as intended
		case 0:
			t.Errorf("key %q was skipped across pages", k)
		default:
			t.Errorf("key %q was returned %d times across pages", k, seen[k])
		}
	}
	if len(seen) != len(keys) {
		t.Errorf("saw %d distinct keys, want %d", len(seen), len(keys))
	}
}

// TestStoreInt_ListObjects_AgreesWithDelimitedOrder asserts the two listing
// paths order the same keys the same way. They are separate queries, and the
// delimited one was collated first, so nothing but a test keeps them aligned.
func TestStoreInt_ListObjects_AgreesWithDelimitedOrder(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	const prefix = "collate-delim/"
	seedCollationKeys(t, s, "listobjects-collate-delim-be", prefix)

	flat := pageAllObjects(t, s, prefix, 100)

	// A delimiter no key contains makes the delimited listing return the same
	// leaves as the flat one, so only the ordering can differ.
	res, err := s.ListObjectsDelimited(ctx, prefix, "|", "", 1000)
	if err != nil {
		t.Fatalf("ListObjectsDelimited: %v", err)
	}
	delimited := make([]string, 0, len(res.Objects))
	for i := range res.Objects {
		delimited = append(delimited, res.Objects[i].ObjectKey)
	}

	if !slices.Equal(flat, delimited) {
		t.Errorf("flat and delimited listings disagree on order:\n flat      %q\n delimited %q",
			flat, delimited)
	}
}
