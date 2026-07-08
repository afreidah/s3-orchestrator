// -------------------------------------------------------------------------------
// Admin API - Object Listing Handler
//
// Author: Alex Freidah
//
// Read-only browse endpoint backing the TUI object browser. Returns one
// delimiter-grouped page of the object namespace straight from the object
// store, mirroring the S3 ListObjectsV2 delimiter semantics.
// -------------------------------------------------------------------------------

package admin

import (
	"log/slog"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// defaultListMaxKeys caps one browse page when the caller sends no limit.
const defaultListMaxKeys = 1000

// handleListObjects returns one delimiter-grouped page under the given prefix.
// The delimiter defaults to "/" so listings are hierarchical; continuation
// resumes a truncated page.
func (h *Handler) handleListObjects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	if delimiter == "" {
		delimiter = "/"
	}
	continuation := q.Get("continuation")

	page, err := h.objects.ListObjectsDelimited(r.Context(), prefix, delimiter, continuation, defaultListMaxKeys)
	if err != nil {
		h.internalError(r.Context(), w, "failed to list objects", err, slog.String("prefix", prefix))
		return
	}

	resp := adminapi.ObjectListResponse{
		CommonPrefixes: page.CommonPrefixes,
		Truncated:      page.IsTruncated,
		Next:           page.NextContinuationToken,
	}
	for i := range page.Objects {
		resp.Objects = append(resp.Objects, adminapi.ObjectEntry{
			Key:  page.Objects[i].ObjectKey,
			Size: page.Objects[i].SizeBytes,
		})
	}

	httputil.WriteJSON(w, http.StatusOK, resp)
}
