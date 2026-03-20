package server

import (
	"fmt"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// Path parsing
// -------------------------------------------------------------------------

func BenchmarkParsePath(b *testing.B) {
	paths := []struct {
		name, path string
	}{
		{"bucket_only", "/mybucket"},
		{"bucket_and_key", "/mybucket/photos/image.jpg"},
		{"deep_path", "/mybucket/a/b/c/d/e/f/g/file.txt"},
	}

	for _, p := range paths {
		b.Run(p.name, func(b *testing.B) {
			for b.Loop() {
				parsePath(p.path)
			}
		})
	}
}

// -------------------------------------------------------------------------
// Request ID validation
// -------------------------------------------------------------------------

func BenchmarkIsValidRequestID(b *testing.B) {
	ids := []struct {
		name, id string
	}{
		{"valid_32", "abcdef0123456789abcdef0123456789"},
		{"valid_64", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"},
		{"invalid_chars", "abcdef012345xyz9abcdef0123456789"},
		{"empty", ""},
	}

	for _, tc := range ids {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				isValidRequestID(tc.id)
			}
		})
	}
}

// -------------------------------------------------------------------------
// Metadata extraction and validation
// -------------------------------------------------------------------------

func BenchmarkExtractUserMetadata(b *testing.B) {
	cases := []struct {
		name    string
		headers int
	}{
		{"no_meta", 0},
		{"3_meta", 3},
		{"10_meta", 10},
		{"50_meta", 50},
	}

	for _, tc := range cases {
		h := make(http.Header)
		// Add some non-meta headers that must be skipped
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Authorization", "AWS4-HMAC-SHA256 ...")
		h.Set("X-Amz-Date", "20260307T000000Z")
		for i := range tc.headers {
			h.Set(fmt.Sprintf("X-Amz-Meta-Key%d", i), fmt.Sprintf("value%d", i))
		}

		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				extractUserMetadata(h)
			}
		})
	}
}

func BenchmarkValidateUserMetadata(b *testing.B) {
	small := map[string]string{"author": "test", "project": "bench"}
	large := make(map[string]string, 20)
	for i := range 20 {
		large[fmt.Sprintf("key-%02d", i)] = fmt.Sprintf("value-%02d-padding", i)
	}

	b.Run("small_2keys", func(b *testing.B) {
		for b.Loop() {
			_ = validateUserMetadata(small)
		}
	})
	b.Run("large_20keys", func(b *testing.B) {
		for b.Loop() {
			_ = validateUserMetadata(large)
		}
	})
}

// -------------------------------------------------------------------------
// S3 XML error response
// -------------------------------------------------------------------------

func BenchmarkWriteS3Error(b *testing.B) {
	for b.Loop() {
		w := httptest.NewRecorder()
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
	}
}

// -------------------------------------------------------------------------
// S3 XML list response encoding
// -------------------------------------------------------------------------

func BenchmarkWriteXML_ListV2(b *testing.B) {
	sizes := []int{10, 100, 1000}

	for _, n := range sizes {
		objects := make([]store.ObjectLocation, n)
		now := time.Now()
		for i := range n {
			objects[i] = store.ObjectLocation{
				ObjectKey: fmt.Sprintf("mybucket/photos/image-%04d.jpg", i),
				SizeBytes: int64(1024 + i),
				CreatedAt: now,
			}
		}
		contents, _ := buildListContents(objects, nil, "mybucket/")

		result := xmlListResultV2{
			Xmlns:    "http://s3.amazonaws.com/doc/2006-03-01/",
			Name:     "mybucket",
			Prefix:   "photos/",
			MaxKeys:  n,
			KeyCount: n,
			Contents: contents,
		}

		b.Run(fmt.Sprintf("%d_objects", n), func(b *testing.B) {
			for b.Loop() {
				w := httptest.NewRecorder()
				_ = writeXML(w, http.StatusOK, result)
			}
		})
	}
}

func BenchmarkBuildListContents(b *testing.B) {
	now := time.Now()
	objects := make([]store.ObjectLocation, 1000)
	for i := range 1000 {
		objects[i] = store.ObjectLocation{
			ObjectKey: fmt.Sprintf("mybucket/dir/subdir/file-%04d.txt", i),
			SizeBytes: int64(i * 100),
			CreatedAt: now,
		}
	}
	prefixes := []string{
		"mybucket/dir/a/",
		"mybucket/dir/b/",
		"mybucket/dir/c/",
	}

	b.Run("1000_objects_3_prefixes", func(b *testing.B) {
		for b.Loop() {
			buildListContents(objects, prefixes, "mybucket/")
		}
	})
}

// -------------------------------------------------------------------------
// Span name and status string allocation
// -------------------------------------------------------------------------

func BenchmarkSpanNameSprintf(b *testing.B) {
	methods := []string{"GET", "PUT", "HEAD", "DELETE", "POST"}
	for b.Loop() {
		for _, m := range methods {
			_ = fmt.Sprintf("HTTP %s", m)
		}
	}
}

func BenchmarkRecordRequestStatusItoa(b *testing.B) {
	codes := []int{200, 204, 400, 403, 404, 502, 503}
	for b.Loop() {
		for _, c := range codes {
			_ = strconv.Itoa(c)
		}
	}
}
