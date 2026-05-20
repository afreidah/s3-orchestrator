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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// dashboardPage holds all data passed to the dashboard template.
type dashboardPage struct {
	Version          string
	DBHealthy        bool
	Data             *dashboard.Data
	Buckets          []string
	Config           configSummary
	Integrity        integrityStats
	TotalBytesUsed   int64
	TotalBytesLimit  int64
	TotalOrphanBytes int64
}

// counterVecTotal sums the current value of every label combination on
// a Prometheus CounterVec. Used by the dashboard to surface integrity
// check / error totals without scraping /metrics. Returns 0 on any
// collection error - the dashboard treats "no data" the same as "zero
// activity," which is the operator-meaningful interpretation.
func counterVecTotal(v *prometheus.CounterVec) float64 {
	ch := make(chan prometheus.Metric, 64)
	v.Collect(ch)
	close(ch)
	var total float64
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			continue
		}
		if c := pb.GetCounter(); c != nil {
			total += c.GetValue()
		}
	}
	return total
}

// configSummary holds non-sensitive configuration for display. The Enabled
// flags drive which admin-action buttons render in the dashboard.
type configSummary struct {
	RoutingStrategy           string
	ReplicationFactor         int
	RebalanceEnabled          bool
	RebalanceStrategy         string
	RateLimitEnabled          bool
	EncryptionEnabled         bool
	IntegrityEnabled          bool
	IntegrityVerifyOnRead     bool
	IntegrityScrubberInterval time.Duration
}

// integrityStats is the dashboard's snapshot of the s3o_integrity_*
// Prometheus counters. Numeric totals are summed across label values
// (operation labels) so the UI shows a single overall count.
type integrityStats struct {
	ChecksTotal float64
	ErrorsTotal float64
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
			RoutingStrategy:           string(cfg.RoutingStrategy),
			ReplicationFactor:         cfg.Replication.Factor,
			RebalanceEnabled:          cfg.Rebalance.Enabled,
			RebalanceStrategy:         cfg.Rebalance.Strategy,
			RateLimitEnabled:          cfg.RateLimit.Enabled,
			EncryptionEnabled:         cfg.Encryption.Enabled,
			IntegrityEnabled:          cfg.Integrity.Enabled,
			IntegrityVerifyOnRead:     cfg.Integrity.VerifyOnRead,
			IntegrityScrubberInterval: cfg.Integrity.ScrubberInterval,
		},
		Integrity: integrityStats{
			ChecksTotal: counterVecTotal(telemetry.IntegrityChecksTotal),
			ErrorsTotal: counterVecTotal(telemetry.IntegrityErrorsTotal),
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
		httputil.WriteJSONError(w, http.StatusInternalServerError, "failed to load data")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, data)
}
