// -------------------------------------------------------------------------------
// Admin API - Object Data Cache Control
//
// Author: Alex Freidah
//
// /admin/api/cache/* endpoints: full flush, per-key invalidate, prefix
// invalidate, and a stats snapshot. Every endpoint reports a 503 with a
// uniform "disabled" body when the orchestrator was started without the
// object data cache configured, so callers can distinguish "no cache" from
// "cache empty."
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// cacheDisabledReason is the body reason emitted when an admin cache
// endpoint is called against an orchestrator started without the
// object data cache configured.
const cacheDisabledReason = "object data cache is not enabled"

// writeCacheDisabled emits the standard 503 response used by every
// /admin/api/cache/* handler when h.objectCache is nil. Centralised so
// the shape ("status: disabled", reason) cannot drift between routes.
func (h *Handler) writeCacheDisabled(w http.ResponseWriter) {
	httputil.WriteJSON(w, http.StatusServiceUnavailable, adminapi.CacheDisabledResponse{
		Status: "disabled",
		Reason: cacheDisabledReason,
	})
}

// handleCacheFlush drops every entry from the in-memory object data
// cache. Returns 503 when the cache is disabled (objectCache nil) so
// callers can distinguish "no cache configured" from "cache empty
// after flush." Used by load-test tooling to characterise cache-cold
// GET performance.
func (h *Handler) handleCacheFlush(w http.ResponseWriter, r *http.Request) {
	if h.objectCache == nil {
		h.writeCacheDisabled(w)
		return
	}

	cleared := h.objectCache.Clear()
	telemetry.CacheFlushTotal.Inc()
	h.log.InfoContext(r.Context(), "admin cache flush", "entries_cleared", cleared)
	httputil.WriteJSON(w, http.StatusOK, adminapi.CacheInvalidateResponse{
		Status:         "flushed",
		EntriesDropped: cleared,
	})
}

// handleCacheStats returns the current object data cache utilization.
// Mirrors the s3o_cache_* gauges so operators without Prometheus
// access can still inspect cache state via the admin API.
func (h *Handler) handleCacheStats(w http.ResponseWriter, _ *http.Request) {
	if h.objectCache == nil {
		h.writeCacheDisabled(w)
		return
	}
	stats := h.objectCache.Stats()
	httputil.WriteJSON(w, http.StatusOK, adminapi.CacheStatsResponse{
		Entries:   stats.Entries,
		SizeBytes: stats.SizeBytes,
		MaxBytes:  stats.MaxBytes,
		Hits:      stats.Hits,
		Misses:    stats.Misses,
	})
}

// handleCacheInvalidateKey drops a single key from the cache. The key
// is taken from the URL path, supporting embedded slashes via the
// `{key...}` wildcard pattern. Always returns 200 - Invalidate is a
// no-op for unknown keys, matching the cache's own contract.
func (h *Handler) handleCacheInvalidateKey(w http.ResponseWriter, r *http.Request) {
	if h.objectCache == nil {
		h.writeCacheDisabled(w)
		return
	}
	key := r.PathValue("key")
	if key == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "key is required")
		return
	}
	h.objectCache.Invalidate(key)
	telemetry.CacheAdminInvalidationsTotal.Inc()
	h.log.InfoContext(r.Context(), "admin cache invalidate", "key", key)
	httputil.WriteJSON(w, http.StatusOK, adminapi.CacheInvalidateKeyResponse{
		Status: "invalidated",
		Key:    key,
	})
}

// handleCacheInvalidatePrefix drops every entry whose key starts with
// the prefix query parameter. Empty prefix is rejected as 400 to keep
// "drop everything" as a deliberate cache-flush call rather than an
// accidentally-empty parameter; operators wanting a full flush should
// use POST /admin/api/cache/flush.
func (h *Handler) handleCacheInvalidatePrefix(w http.ResponseWriter, r *http.Request) {
	if h.objectCache == nil {
		h.writeCacheDisabled(w)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest,
			"prefix query parameter is required (use POST /admin/api/cache/flush to drop every entry)")
		return
	}
	dropped := h.objectCache.InvalidatePrefix(prefix)
	telemetry.CacheAdminInvalidationsTotal.Add(float64(dropped))
	h.log.InfoContext(r.Context(), "admin cache invalidate prefix",
		"prefix", prefix, "entries_dropped", dropped)
	httputil.WriteJSON(w, http.StatusOK, adminapi.CacheInvalidateResponse{
		Status:         "invalidated",
		Prefix:         prefix,
		EntriesDropped: dropped,
	})
}
