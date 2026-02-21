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

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/storage"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// Handler serves the web UI dashboard.
type Handler struct {
	manager   *storage.BackendManager
	dbHealthy func() bool
	cfg       *config.Config
	templates *template.Template
}

// New creates a new UI handler.
func New(manager *storage.BackendManager, dbHealthy func() bool, cfg *config.Config) *Handler {
	return &Handler{
		manager:   manager,
		dbHealthy: dbHealthy,
		cfg:       cfg,
		templates: loadTemplates(),
	}
}

// Register mounts the UI routes on the given mux under the configured prefix.
func (h *Handler) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc(prefix+"/", h.handleDashboard)
	mux.HandleFunc(prefix+"/api/dashboard", h.handleAPIDashboard)
	mux.Handle(prefix+"/static/", http.StripPrefix(prefix+"/static/", http.FileServerFS(staticFS)))
}

// dashboardPage holds all data passed to the dashboard template.
type dashboardPage struct {
	Version   string
	DBHealthy bool
	Data      *storage.DashboardData
	Buckets   []string
	Config    configSummary
}

// configSummary holds non-sensitive configuration for display.
type configSummary struct {
	ReplicationFactor int
	RebalanceEnabled  bool
	RebalanceStrategy string
	RateLimitEnabled  bool
}

// handleDashboard renders the HTML dashboard page.
func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := h.manager.GetDashboardData(r.Context())
	if err != nil {
		slog.Error("UI: failed to get dashboard data", "error", err)
		http.Error(w, "Failed to load dashboard data", http.StatusInternalServerError)
		return
	}

	bucketNames := make([]string, len(h.cfg.Buckets))
	for i, b := range h.cfg.Buckets {
		bucketNames[i] = b.Name
	}

	page := dashboardPage{
		Version:   telemetry.Version,
		DBHealthy: h.dbHealthy(),
		Data:      data,
		Buckets:   bucketNames,
		Config: configSummary{
			ReplicationFactor: h.cfg.Replication.Factor,
			RebalanceEnabled:  h.cfg.Rebalance.Enabled,
			RebalanceStrategy: h.cfg.Rebalance.Strategy,
			RateLimitEnabled:  h.cfg.RateLimit.Enabled,
		},
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, "dashboard.html", page); err != nil {
		slog.Error("UI: failed to render dashboard", "error", err)
	}
}

// handleAPIDashboard returns dashboard data as JSON.
func (h *Handler) handleAPIDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := h.manager.GetDashboardData(r.Context())
	if err != nil {
		slog.Error("UI: failed to get dashboard data", "error", err)
		http.Error(w, `{"error":"failed to load data"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
