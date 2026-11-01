// -------------------------------------------------------------------------------
// UI Handler - Dashboard HTML and JSON
//
// Author: Alex Freidah
//
// Server-side rendered HTML dashboard plus its JSON twin. Both start from the
// same dashboard data, with the object tree's root cut to the buckets the user
// may list and every declared one shown even when empty. The HTML renderer then
// folds in the configuration summary the template needs to decide which
// admin-action buttons to render. Interactive form controls in the template
// carry aria-labels so the rendered page passes accessibility checks.
// -------------------------------------------------------------------------------

package ui

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

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
	CompressionEnabled        bool
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

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// handleDashboard renders the HTML dashboard page.
func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data, buckets, err := h.dashboardData(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "failed to get dashboard data", "error", err)
		http.Error(w, "Failed to load dashboard data", http.StatusInternalServerError)
		return
	}

	cfg := h.d.Config.Load()

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
		DBHealthy:        h.d.DBHealthy(),
		Data:             data,
		Buckets:          buckets,
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
			CompressionEnabled:        cfg.Compression.Enabled,
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
	data, _, err := h.dashboardData(r.Context())
	if err != nil {
		httputil.InternalError(r.Context(), h.log, w, "failed to load data", err)
		return
	}

	httputil.WriteJSON(w, http.StatusOK, data)
}

// dashboardData loads the dashboard figures with the root of the object tree
// cut to the buckets this user may list, each declared one shown even when it
// holds nothing yet, and returns those bucket names for the page's pickers.
func (h *Handler) dashboardData(ctx context.Context) (*dashboard.Data, []string, error) {
	data, err := h.d.Dashboard.GetData(ctx)
	if err != nil {
		return nil, nil, err
	}
	buckets := slices.DeleteFunc(h.d.Buckets.Names(), func(name string) bool { return !canList(ctx, name) })

	root := listable(ctx, data.TopLevelEntries.Entries)
	for _, name := range buckets {
		if !slices.ContainsFunc(root, func(e core.DirEntry) bool { return e.Name == name+"/" }) {
			root = append(root, core.DirEntry{Name: name + "/", IsDir: true})
		}
	}
	data.TopLevelEntries.Entries = root
	return data, buckets, nil
}

// listable keeps the root entries naming a bucket this user may list, the one
// filter the dashboard tree and the tree API both apply to the root.
func listable(ctx context.Context, entries []core.DirEntry) []core.DirEntry {
	return slices.DeleteFunc(entries, func(e core.DirEntry) bool {
		return !canList(ctx, strings.TrimSuffix(e.Name, "/"))
	})
}
