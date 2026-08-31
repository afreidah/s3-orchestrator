// -------------------------------------------------------------------------------
// Admin API - Bulk Rewrite Handler Tests
//
// Author: Alex Freidah
//
// The four fleet-wide rewrite endpoints. What the handler owes its caller is a
// truthful tally - and the distinction between an object it declined and one it
// failed on, since a pass over media legitimately skips almost everything and
// must not read as broken.
//
// All four go through one handler, so the streaming and cap behaviour is
// asserted across every endpoint rather than on the compression pair alone.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// emptyCompressionStore serves no rows, so a pass completes having considered
// nothing.
type emptyCompressionStore struct{}

// ListUncompressedLocations returns no rows.
func (emptyCompressionStore) ListUncompressedLocations(context.Context, int, core.Cursor, core.CompressionThresholds) ([]core.RewritableLocation, error) {
	return nil, nil
}

// RecordCompressionProbe is never reached: the listing is empty.
func (emptyCompressionStore) RecordCompressionProbe(context.Context, *core.CompressionProbe) error {
	return nil
}

// ListCompressedLocations returns no rows.
func (emptyCompressionStore) ListCompressedLocations(context.Context, int, core.Cursor) ([]core.RewritableLocation, error) {
	return nil, nil
}

// MarkObjectCompressed is never reached with no rows to rewrite.
func (emptyCompressionStore) MarkObjectCompressed(context.Context, *core.CompressedUpdate, int64) error {
	return nil
}

// postBulkRewrite drives one endpoint and returns the recorder.
func postBulkRewrite(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, nil)
	req.Header.Set("X-Admin-Token", "test-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestCompressExisting_NoCodec verifies a deployment with no codec is told so,
// rather than being handed a run that silently did nothing.
func TestCompressExisting_NoCodec(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)

	for _, path := range []string{"/admin/api/compress-existing", "/admin/api/decompress-existing"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			if w := postBulkRewrite(t, h, path); w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

// TestCompressExisting_EmptyFleet verifies a pass with nothing to do reports a
// complete run of zero rather than an error.
func TestCompressExisting_EmptyFleet(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	codec, err := compression.NewCodec(compression.DefaultLevel, compression.MinChunkSize)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(codec.Close)
	compressionWith(t, h, codec, emptyCompressionStore{})

	w := postBulkRewrite(t, h, "/admin/api/compress-existing")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var got adminapi.CompressExistingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Status != statusComplete {
		t.Errorf("status = %q, want %q", got.Status, statusComplete)
	}
	if got.Compressed != 0 || got.Skipped != 0 || got.Failed != 0 || got.Total != 0 {
		t.Errorf("counts = %+v, want all zero", got)
	}
}

// TestDecompressExisting_EmptyFleet covers the reverse endpoint's wire shape,
// which names its success count differently.
func TestDecompressExisting_EmptyFleet(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	codec, err := compression.NewCodec(compression.DefaultLevel, compression.MinChunkSize)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(codec.Close)
	compressionWith(t, h, codec, emptyCompressionStore{})

	w := postBulkRewrite(t, h, "/admin/api/decompress-existing")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var got adminapi.DecompressExistingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Status != statusComplete || got.Decompressed != 0 {
		t.Errorf("response = %+v, want a complete run of zero", got)
	}
}

// TestCompressExisting_StreamsProgress checks the endpoint answers with an
// NDJSON event stream when the caller asks for one. These passes read and
// rewrite an entire fleet, so a caller watching one has to see it move; a
// single JSON body at the end is indistinguishable from a hung request until
// it arrives.
func TestCompressExisting_StreamsProgress(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	codec, err := compression.NewCodec(compression.DefaultLevel, compression.MinChunkSize)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(codec.Close)
	compressionWith(t, h, codec, emptyCompressionStore{})

	for _, path := range []string{"/admin/api/compress-existing", "/admin/api/decompress-existing"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			h.Register(mux)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, nil)
			req.Header.Set("X-Admin-Token", "test-token")
			req.Header.Set("Accept", adminstream.ContentType)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
			}
			if got := w.Header().Get("Content-Type"); got != adminstream.ContentType {
				t.Errorf("Content-Type = %q, want %q", got, adminstream.ContentType)
			}
			// A completed pass brackets its work with a start and a result
			// event, which is what the TUI renders around the per-object steps.
			// The summary names skipped objects separately, since a pass over
			// media declines almost everything.
			body := w.Body.String()
			for _, want := range []string{`"event":"start"`, `"event":"result"`, "skipped"} {
				if !strings.Contains(body, want) {
					t.Errorf("stream body %q does not contain %s", body, want)
				}
			}
		})
	}
}

// TestEncryptExisting_EmptyFleet pins the encryption pair's wire shape. Both
// now report skipped alongside failed: the driver declines copies on backends
// over their usage limit, and until these shared one handler that count was
// tallied and then dropped on the floor.
func TestEncryptExisting_EmptyFleet(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	encryptionWith(t, h, testEncryptor(t), emptyEncryptionStore(t))

	w := postBulkRewrite(t, h, "/admin/api/encrypt-existing")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var got adminapi.EncryptExistingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Status != statusComplete {
		t.Errorf("status = %q, want %q", got.Status, statusComplete)
	}
	if got.Encrypted != 0 || got.Skipped != 0 || got.Failed != 0 || got.Total != 0 {
		t.Errorf("counts = %+v, want all zero", got)
	}
}

// TestDecryptExisting_EmptyFleet covers the reverse endpoint's wire shape.
func TestDecryptExisting_EmptyFleet(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	encryptionWith(t, h, testEncryptor(t), emptyEncryptionStore(t))

	w := postBulkRewrite(t, h, "/admin/api/decrypt-existing")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var got adminapi.DecryptExistingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Status != statusComplete || got.Decrypted != 0 {
		t.Errorf("response = %+v, want a complete run of zero", got)
	}
}

// TestEncryptExisting_StreamsProgress checks the encryption passes stream the
// same way the compression ones do. They rewrite the same fleet and take just
// as long, and until they shared a handler only half of them could be watched.
func TestEncryptExisting_StreamsProgress(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	encryptionWith(t, h, testEncryptor(t), emptyEncryptionStore(t))

	for _, path := range []string{"/admin/api/encrypt-existing", "/admin/api/decrypt-existing"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			h.Register(mux)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, nil)
			req.Header.Set("X-Admin-Token", "test-token")
			req.Header.Set("Accept", adminstream.ContentType)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
			}
			if got := w.Header().Get("Content-Type"); got != adminstream.ContentType {
				t.Errorf("Content-Type = %q, want %q", got, adminstream.ContentType)
			}
			body := w.Body.String()
			for _, want := range []string{`"event":"start"`, `"event":"result"`} {
				if !strings.Contains(body, want) {
					t.Errorf("stream body %q does not contain %s", body, want)
				}
			}
		})
	}
}
