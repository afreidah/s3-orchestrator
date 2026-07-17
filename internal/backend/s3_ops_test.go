// -------------------------------------------------------------------------------
// Backend Tests - S3 CRUD Operations
//
// Author: Alex Freidah
//
// Exercises GetObject, HeadObject, DeleteObject, ListObjects, and CopyObject
// against an httptest fake-S3 endpoint so the SDK request/response mapping
// (attribute unwrapping, list-page conversion, copy-result ETag) is covered
// without a live provider.
// -------------------------------------------------------------------------------

package backend

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// newTestBackend spins up an httptest server with handler and returns an
// S3Backend pointed at it (path-style, signed payload). The server is closed
// via t.Cleanup.
func newTestBackend(t *testing.T, handler http.HandlerFunc) *S3Backend {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	be, err := NewS3Backend(t.Context(), &config.BackendConfig{
		Name:            "test",
		Endpoint:        srv.URL,
		Region:          "us-east-1",
		Bucket:          "test-bucket",
		AccessKeyID:     "AKID",
		SecretAccessKey: "secret",
		ForcePathStyle:  true,
		DisableChecksum: true,
	})
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}
	return be
}

// TestGetObject covers the GET path and the attribute unwrapping in
// mapGetObjectResult / objectAttrsFromSDK.
func TestGetObject(t *testing.T) {
	t.Parallel()
	payload := []byte("get-object-body")
	be := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("ETag", `"etag123"`)
		w.Header().Set("Last-Modified", "Wed, 01 Jan 2026 00:00:00 GMT")
		w.Header().Set("x-amz-meta-owner", "alex")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	})

	res, err := be.GetObject(t.Context(), "dir/obj", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer res.Body.Close()

	if got, _ := io.ReadAll(res.Body); !bytes.Equal(got, payload) {
		t.Errorf("body = %q, want %q", got, payload)
	}
	if res.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", res.Size, len(payload))
	}
	if res.ETag != `"etag123"` {
		t.Errorf("etag = %q, want %q", res.ETag, `"etag123"`)
	}
	if res.ContentType != "text/plain" {
		t.Errorf("contentType = %q, want text/plain", res.ContentType)
	}
	if res.Metadata["owner"] != "alex" {
		t.Errorf("metadata = %v, want owner=alex", res.Metadata)
	}
}

// TestHeadObject covers the HEAD path and its attribute unwrapping.
func TestHeadObject(t *testing.T) {
	t.Parallel()
	be := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		w.Header().Set("Content-Length", "42")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"headetag"`)
		w.WriteHeader(http.StatusOK)
	})

	res, err := be.HeadObject(t.Context(), "dir/obj")
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if res.Size != 42 {
		t.Errorf("size = %d, want 42", res.Size)
	}
	if res.ETag != `"headetag"` {
		t.Errorf("etag = %q, want %q", res.ETag, `"headetag"`)
	}
}

// TestDeleteObject covers the DELETE path.
func TestDeleteObject(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	be := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	if err := be.DeleteObject(t.Context(), "dir/obj"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if gotPath != "/test-bucket/dir/obj" {
		t.Errorf("path = %q, want /test-bucket/dir/obj", gotPath)
	}
}

// TestListObjects covers the ListObjectsV2 walk and convertListPage.
func TestListObjects(t *testing.T) {
	t.Parallel()
	const body = `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>test-bucket</Name>
  <KeyCount>2</KeyCount>
  <MaxKeys>1000</MaxKeys>
  <IsTruncated>false</IsTruncated>
  <Contents><Key>a.txt</Key><Size>3</Size><ETag>&quot;e1&quot;</ETag><LastModified>2026-01-01T00:00:00.000Z</LastModified></Contents>
  <Contents><Key>b.txt</Key><Size>5</Size><ETag>&quot;e2&quot;</ETag><LastModified>2026-01-01T00:00:00.000Z</LastModified></Contents>
</ListBucketResult>`
	be := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	})

	var got []ListedObject
	err := be.ListObjects(t.Context(), "", func(page []ListedObject) error {
		got = append(got, page...)
		return nil
	})
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d objects, want 2: %+v", len(got), got)
	}
	if got[0].Key != "a.txt" || got[0].SizeBytes != 3 {
		t.Errorf("obj[0] = %+v, want {a.txt 3}", got[0])
	}
	if got[1].Key != "b.txt" || got[1].SizeBytes != 5 {
		t.Errorf("obj[1] = %+v, want {b.txt 5}", got[1])
	}
}

// TestCopyObject covers the copy path and CopyObjectResult ETag extraction.
func TestCopyObject(t *testing.T) {
	t.Parallel()
	const body = `<?xml version="1.0" encoding="UTF-8"?>
<CopyObjectResult><ETag>&quot;copyetag&quot;</ETag><LastModified>2026-01-01T00:00:00.000Z</LastModified></CopyObjectResult>`
	var gotCopySource string
	be := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		gotCopySource = r.Header.Get("x-amz-copy-source")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, body)
	})

	etag, err := be.CopyObject(t.Context(), "src/key", "dst/key", "text/plain", nil)
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if etag != `"copyetag"` {
		t.Errorf("etag = %q, want %q", etag, `"copyetag"`)
	}
	if gotCopySource == "" {
		t.Error("expected an x-amz-copy-source header on the copy request")
	}
}
