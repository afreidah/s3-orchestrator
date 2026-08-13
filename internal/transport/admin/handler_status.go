// -------------------------------------------------------------------------------
// Admin API - Read-Only Observability Endpoints
//
// Author: Alex Freidah
//
// Endpoints that surface state without changing it (status, reload-status,
// object-locations, cleanup-queue, workers) plus the small admin
// "control knob" endpoints (usage-flush, log-level) that fit naturally
// alongside them. Worker health is here because it is the snapshot read
// path for the lifecycle supervisor.
// -------------------------------------------------------------------------------

package admin

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// reloadNotYetStatus is the reload-status placeholder reported before the
// first SIGHUP of the process.
const reloadNotYetStatus = "no_reload_yet"

// handleReloadStatus returns the most recent reload result captured by
// the reload coordinator. Returns a "no_reload_yet" placeholder when
// SIGHUP has not fired since startup or when the runtime has not
// wired the provider.
func (h *Handler) handleReloadStatus(w http.ResponseWriter, _ *http.Request) {
	if h.reloadStatus == nil {
		httputil.WriteJSON(w, http.StatusOK, adminapi.ReloadStatusResponse{Status: reloadNotYetStatus})
		return
	}
	result := h.reloadStatus()
	if result == nil {
		httputil.WriteJSON(w, http.StatusOK, adminapi.ReloadStatusResponse{Status: reloadNotYetStatus})
		return
	}
	httputil.WriteJSON(w, http.StatusOK, result)
}

// handleStatus returns backend health and circuit breaker state.
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	data, err := h.dashboardOps.GetData(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch status", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.StatusResponse{
		DBHealthy:   h.dbHealthy(),
		UsagePeriod: data.UsagePeriod,
		Backends:    backendStatuses(data),
		Integrity: adminapi.IntegrityStatus{
			OldestUnverifiedSeconds: int64(data.OldestUnverifiedAge.Seconds()),
			NeverVerifiedCopies:     data.NeverVerifiedCopies,
			PlaintextCopies:         data.PlaintextCopies,
		},
	})
}

// backendStatuses maps a dashboard snapshot onto the shared status wire type,
// one entry per backend in display order. A backend absent from UnhealthyBackends
// is healthy; presence in DrainingBackends marks it draining.
func backendStatuses(data *dashboard.Data) []adminapi.BackendStatus {
	backends := make([]adminapi.BackendStatus, 0, len(data.BackendOrder))
	for _, name := range data.BackendOrder {
		_, draining := data.DrainingBackends[name]
		bs := adminapi.BackendStatus{
			Name:     name,
			Healthy:  !data.UnhealthyBackends[name],
			Draining: draining,
		}
		if qs, ok := data.QuotaStats[name]; ok {
			bs.BytesUsed = qs.BytesUsed
			bs.BytesLimit = qs.BytesLimit
		}
		if oc, ok := data.ObjectCounts[name]; ok {
			bs.ObjectCount = oc
		}
		if us, ok := data.UsageStats[name]; ok {
			bs.APIRequests = us.APIRequests
			bs.EgressBytes = us.EgressBytes
			bs.IngressBytes = us.IngressBytes
		}
		backends = append(backends, bs)
	}
	return backends
}

// handleObjectLocations returns all copies of an object across backends.
func (h *Handler) handleObjectLocations(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "key parameter is required")
		return
	}

	locations, err := h.objects.GetAllObjectLocations(r.Context(), key)
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch locations", err, slog.String("key", key))
		return
	}

	httputil.WriteJSON(w, http.StatusOK, objectLocationsResponse(key, locations))
}

// objectLocationsResponse maps the store's location ledger onto the shared wire
// type, dropping the raw envelope encryption key so the secret never leaves the
// process.
func objectLocationsResponse(key string, locations []core.ObjectLocation) adminapi.ObjectLocationsResponse {
	resp := adminapi.ObjectLocationsResponse{Key: key}
	for i := range locations {
		resp.Locations = append(resp.Locations, adminapi.ObjectLocation{
			Backend:        locations[i].BackendName,
			SizeBytes:      locations[i].SizeBytes,
			CreatedAt:      locations[i].CreatedAt,
			Encrypted:      locations[i].Encrypted,
			KeyID:          locations[i].KeyID,
			PlaintextSize:  locations[i].PlaintextSize,
			ContentHash:    locations[i].ContentHash,
			LastScrubbedAt: locations[i].LastScrubbedAt,
		})
	}
	return resp
}

// handleCleanupQueue returns cleanup queue depth and pending items.
func (h *Handler) handleCleanupQueue(w http.ResponseWriter, r *http.Request) {
	depth, err := h.cleanup.CleanupQueueDepth(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch cleanup queue", err)
		return
	}

	items, err := h.cleanup.GetPendingCleanups(r.Context(), 50)
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch cleanup queue", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.CleanupQueueResponse{
		Depth: depth,
		Items: cleanupQueueItems(items),
	})
}

// cleanupQueueItems maps the store rows onto the shared wire type. The claim
// pointers flatten to omitted fields so an unclaimed row does not emit nulls.
func cleanupQueueItems(items []core.CleanupItem) []adminapi.CleanupQueueItem {
	out := make([]adminapi.CleanupQueueItem, 0, len(items))
	for i := range items {
		item := adminapi.CleanupQueueItem{
			ID:        items[i].ID,
			Backend:   items[i].BackendName,
			ObjectKey: items[i].ObjectKey,
			Reason:    items[i].Reason,
			SizeBytes: items[i].SizeBytes,
			Attempts:  items[i].Attempts,
			ClaimedAt: items[i].ClaimedAt,
		}
		if items[i].ClaimedBy != nil {
			item.ClaimedBy = *items[i].ClaimedBy
		}
		out = append(out, item)
	}
	return out
}

// handleUsageFlush forces a flush of usage counters to the database.
func (h *Handler) handleUsageFlush(w http.ResponseWriter, r *http.Request) {
	if err := h.backendOps.FlushUsage(r.Context()); err != nil {
		h.internalError(r.Context(), w, "flush failed", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, adminapi.UsageFlushResponse{Status: "flushed"})
}

// handleReconcileUsage recomputes each backend's bytes_used from the object
// ledger and returns the per-backend byte corrections that were applied.
func (h *Handler) handleReconcileUsage(w http.ResponseWriter, r *http.Request) {
	adjustments, err := h.backendOps.ReconcileUsage(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "usage reconcile failed", err)
		return
	}

	audit.Log(r.Context(), "usage.reconcile",
		slog.Int("backends_corrected", len(adjustments)))
	httputil.WriteJSON(w, http.StatusOK, adminapi.UsageReconcileResponse{
		Status:      "reconciled",
		Adjustments: adjustments,
	})
}

// handleLogLevel gets or sets the runtime log level.
func (h *Handler) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		httputil.WriteJSON(w, http.StatusOK, adminapi.LogLevelResponse{
			Level: strings.ToLower(h.logLevel.Level().String()),
		})
		return
	}

	var req adminapi.SetLogLevelRequest
	if !httputil.DecodeJSONBody(w, r, &req, 1<<20) {
		return
	}
	parsed := config.ParseLogLevel(req.Level)
	h.logLevel.Set(parsed)
	h.log.InfoContext(r.Context(), "log level changed via admin API", "level", req.Level)
	httputil.WriteJSON(w, http.StatusOK, adminapi.LogLevelResponse{Level: strings.ToLower(parsed.String())})
}

// handleWorkers returns a snapshot of every registered background
// service's last-tick health. The supervisor records a tick outcome
// after every fire, so operators can identify stalled or repeatedly
// failing workers without scraping logs. Returns 503 when the
// lifecycle manager was not wired (proxy-only deployments that disable
// the worker pool).
func (h *Handler) handleWorkers(w http.ResponseWriter, _ *http.Request) {
	if h.workerHealth == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "worker health not available")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.WorkersResponse{Workers: h.workerHealth()})
}
