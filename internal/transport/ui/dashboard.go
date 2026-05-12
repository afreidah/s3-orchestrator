// -------------------------------------------------------------------------------
// UI Handler - Dashboard HTML and JSON
//
// Author: Alex Freidah
//
// Server-side rendered HTML dashboard plus its JSON twin. The HTML
// renderer composes the page from the same dashboard data the API
// returns, then folds in the configuration summary the template needs
// to decide which admin-action buttons to render and ensures every
// configured bucket shows up as a top-level directory even when empty.
// -------------------------------------------------------------------------------

package ui

import (
	"bytes"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// dashboardPage holds all data passed to the dashboard template.
type dashboardPage struct {
	Version          string
	DBHealthy        bool
	Data             *dashboard.Data
	Buckets          []string
	Config           configSummary
	TotalBytesUsed   int64
	TotalBytesLimit  int64
	TotalOrphanBytes int64
}

// configSummary holds non-sensitive configuration for display. The Enabled
// flags drive which admin-action buttons render in the dashboard.
type configSummary struct {
	RoutingStrategy   string
	ReplicationFactor int
	RebalanceEnabled  bool
	RebalanceStrategy string
	RateLimitEnabled  bool
	EncryptionEnabled bool
	IntegrityEnabled  bool
}

// handleDashboard renders the HTML dashboard page.
func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	data, err := h.backendOps.GetDashboardData(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "failed to get dashboard data", "error", err)
		http.Error(w, "Failed to load dashboard data", http.StatusInternalServerError)
		return
	}

	cfg := h.cfg.Load()
	bucketNames := make([]string, len(cfg.Buckets))
	for i, b := range cfg.Buckets {
		bucketNames[i] = b.Name
	}

	// Ensure every configured bucket appears as a top-level directory in the
	// object tree, even when the bucket has no files yet.
	existing := make(map[string]bool, len(data.TopLevelEntries.Entries))
	for _, e := range data.TopLevelEntries.Entries {
		existing[e.Name] = true
	}
	for _, name := range bucketNames {
		dirName := name + "/"
		if !existing[dirName] {
			data.TopLevelEntries.Entries = append(data.TopLevelEntries.Entries, core.DirEntry{
				Name:  dirName,
				IsDir: true,
			})
		}
	}

	var totalUsed, totalLimit, totalOrphan int64
	unlimited := false
	for _, stat := range data.QuotaStats {
		totalUsed += stat.BytesUsed
		totalOrphan += stat.OrphanBytes
		if stat.BytesLimit == 0 {
			unlimited = true
		}
		totalLimit += stat.BytesLimit
	}
	if unlimited {
		totalLimit = 0
	}

	page := dashboardPage{
		Version:          telemetry.Version,
		DBHealthy:        h.dbHealthy(),
		Data:             data,
		Buckets:          bucketNames,
		TotalBytesUsed:   totalUsed,
		TotalBytesLimit:  totalLimit,
		TotalOrphanBytes: totalOrphan,
		Config: configSummary{
			RoutingStrategy:   string(cfg.RoutingStrategy),
			ReplicationFactor: cfg.Replication.Factor,
			RebalanceEnabled:  cfg.Rebalance.Enabled,
			RebalanceStrategy: cfg.Rebalance.Strategy,
			RateLimitEnabled:  cfg.RateLimit.Enabled,
			EncryptionEnabled: cfg.Encryption.Enabled,
			IntegrityEnabled:  cfg.Integrity.Enabled,
		},
	}

	var buf bytes.Buffer
	if err := h.templates.ExecuteTemplate(&buf, "dashboard.html", page); err != nil {
		h.log.ErrorContext(r.Context(), "failed to render dashboard", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set(headerContentType, contentTypeHTML)
	_, _ = buf.WriteTo(w)
}

// handleAPIDashboard returns dashboard data as JSON.
func (h *Handler) handleAPIDashboard(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	data, err := h.backendOps.GetDashboardData(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "failed to get dashboard data", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to load data")
		return
	}

	writeJSON(w, http.StatusOK, data)
}
