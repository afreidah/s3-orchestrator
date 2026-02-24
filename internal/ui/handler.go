// -------------------------------------------------------------------------------
// UI Handler - Built-in Web Dashboard
//
// Author: Alex Freidah
//
// HTTP handler for the built-in operational dashboard. Renders a server-side
// HTML page showing backend quota stats, monthly usage, and configuration
// summary. Also provides a JSON API endpoint for programmatic access.
// -------------------------------------------------------------------------------

package ui

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/storage"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// Handler serves the web UI dashboard.
type Handler struct {
	manager   *storage.BackendManager
	dbHealthy func() bool
	cfg       atomic.Pointer[config.Config]
	templates *template.Template
}

// New creates a new UI handler.
func New(manager *storage.BackendManager, dbHealthy func() bool, cfg *config.Config) *Handler {
	h := &Handler{
		manager:   manager,
		dbHealthy: dbHealthy,
		templates: loadTemplates(),
	}
	h.cfg.Store(cfg)
	return h
}

// UpdateConfig atomically replaces the config used by the dashboard.
// Called on SIGHUP to keep the dashboard in sync with the running config.
func (h *Handler) UpdateConfig(cfg *config.Config) {
	h.cfg.Store(cfg)
}

// setSecurityHeaders adds security headers to dashboard responses.
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'")
}

// Register mounts the UI routes on the given mux under the configured prefix.
func (h *Handler) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc(prefix+"/", h.handleDashboard)
	mux.HandleFunc(prefix+"/api/dashboard", h.handleAPIDashboard)
	mux.HandleFunc(prefix+"/api/tree", h.handleTreeAPI)
	mux.Handle(prefix+"/static/", http.StripPrefix(prefix+"/static/", http.FileServerFS(staticFS)))
}

// dashboardPage holds all data passed to the dashboard template.
type dashboardPage struct {
	Version        string
	DBHealthy      bool
	Data           *storage.DashboardData
	Buckets        []string
	Config         configSummary
	TotalBytesUsed int64
	TotalBytesLimit int64
}

// configSummary holds non-sensitive configuration for display.
type configSummary struct {
	RoutingStrategy   string
	ReplicationFactor int
	RebalanceEnabled  bool
	RebalanceStrategy string
	RateLimitEnabled  bool
}

// handleDashboard renders the HTML dashboard page.
func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	data, err := h.manager.GetDashboardData(r.Context())
	if err != nil {
		slog.Error("UI: failed to get dashboard data", "error", err)
		http.Error(w, "Failed to load dashboard data", http.StatusInternalServerError)
		return
	}

	cfg := h.cfg.Load()
	bucketNames := make([]string, len(cfg.Buckets))
	for i, b := range cfg.Buckets {
		bucketNames[i] = b.Name
	}

	var totalUsed, totalLimit int64
	unlimited := false
	for _, stat := range data.QuotaStats {
		totalUsed += stat.BytesUsed
		if stat.BytesLimit == 0 {
			unlimited = true
		}
		totalLimit += stat.BytesLimit
	}
	if unlimited {
		totalLimit = 0
	}

	page := dashboardPage{
		Version:         telemetry.Version,
		DBHealthy:       h.dbHealthy(),
		Data:            data,
		Buckets:         bucketNames,
		TotalBytesUsed:  totalUsed,
		TotalBytesLimit: totalLimit,
		Config: configSummary{
			RoutingStrategy:   cfg.RoutingStrategy,
			ReplicationFactor: cfg.Replication.Factor,
			RebalanceEnabled:  cfg.Rebalance.Enabled,
			RebalanceStrategy: cfg.Rebalance.Strategy,
			RateLimitEnabled:  cfg.RateLimit.Enabled,
		},
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, "dashboard.html", page); err != nil {
		slog.Error("UI: failed to render dashboard", "error", err)
	}
}

// handleAPIDashboard returns dashboard data as JSON.
func (h *Handler) handleAPIDashboard(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	data, err := h.manager.GetDashboardData(r.Context())
	if err != nil {
		slog.Error("UI: failed to get dashboard data", "error", err)
		http.Error(w, `{"error":"failed to load data"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("UI: failed to encode dashboard JSON", "error", err)
	}
}

// handleTreeAPI returns children of a directory prefix as JSON for the
// lazy-loaded file browser.
func (h *Handler) handleTreeAPI(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	prefix := r.URL.Query().Get("prefix")
	startAfter := r.URL.Query().Get("startAfter")
	maxKeys := 200
	if mk := r.URL.Query().Get("maxKeys"); mk != "" {
		if parsed, err := strconv.Atoi(mk); err == nil && parsed > 0 && parsed <= 200 {
			maxKeys = parsed
		}
	}

	result, err := h.manager.GetDirectoryChildren(r.Context(), prefix, startAfter, maxKeys)
	if err != nil {
		slog.Error("UI: failed to list directory children", "prefix", prefix, "error", err)
		http.Error(w, `{"error":"failed to list children"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		slog.Error("UI: failed to encode tree JSON", "error", err)
	}
}
