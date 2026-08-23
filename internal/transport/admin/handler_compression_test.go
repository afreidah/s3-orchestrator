// -------------------------------------------------------------------------------
// Admin API - Compression Handler Tests
//
// Author: Alex Freidah
//
// The two bulk compression endpoints. What the handler owes its caller is a
// truthful tally - and the distinction between an object it declined and one it
// failed on, since a pass over media legitimately skips almost everything and
// must not read as broken.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// emptyCompressionStore serves no rows, so a pass completes having considered
// nothing.
type emptyCompressionStore struct{}

// ListUncompressedLocations returns no rows.
func (emptyCompressionStore) ListUncompressedLocations(context.Context, int, int) ([]core.RewritableLocation, error) {
	return nil, nil
}

// ListCompressedLocations returns no rows.
func (emptyCompressionStore) ListCompressedLocations(context.Context, int, int) ([]core.RewritableLocation, error) {
	return nil, nil
}

// MarkObjectCompressed is never reached with no rows to rewrite.
func (emptyCompressionStore) MarkObjectCompressed(context.Context, *core.CompressedUpdate, int64) error {
	return nil
}

// postCompression drives one endpoint and returns the recorder.
func postCompression(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
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
			if w := postCompression(t, h, path); w.Code != http.StatusBadRequest {
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

	w := postCompression(t, h, "/admin/api/compress-existing")
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

	w := postCompression(t, h, "/admin/api/decompress-existing")
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
