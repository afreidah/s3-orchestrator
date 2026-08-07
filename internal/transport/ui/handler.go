// -------------------------------------------------------------------------------
// UI Handler - Type, Constructor, and Package-Level Surface
//
// Author: Alex Freidah
//
// Hosts the Handler type, the constructor, and the small helpers every
// other ui_*.go file consumes (session key derivation, client IP
// resolution from trusted proxies, bucket/backend validation, security
// headers, package-level constants). Each domain area lives in a
// focused sibling file:
//
//   - routes.go         route registration + audit table
//   - auth.go           session/CSRF/login/logout
//   - dashboard.go      dashboard HTML + JSON
//   - objects.go        upload/download/delete/tree
//   - admin_ops.go      rebalance/clean-excess/sync (async ops + status)
//   - admin_actions.go  replicate/scrub/backfill/encrypt (async-action helpers)
//   - logs.go           log-API handler and query parsers
//   - responses.go      shared JSON read/write helpers
//   - async.go          asyncOpTracker (shared by admin_ops and admin_actions)
//   - templates.go      template loading + embedded static FS
// -------------------------------------------------------------------------------

// Package ui provides the built-in web dashboard for operational visibility,
// serving HTML pages, JSON API endpoints, and static assets.
package ui

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/object"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// sessionCookieName and related constants used by this package.
const (
	sessionCookieName = "s3orch_session"
	csrfCookieName    = "s3orch_csrf"
	csrfHeaderName    = "X-CSRF-Token"
	sessionTTL        = 24 * time.Hour

	contentTypeJSON      = "application/json"
	contentTypeHTML      = "text/html; charset=utf-8"
	headerContentType    = "Content-Type"
	loginPath            = "/login"
	errKeyRequired       = "key is required"
	errLoginRenderFailed = "failed to render login page"
	opCleanExcess        = "clean-excess"
)

// BackendSyncer is the backend-sync surface the UI's admin actions pane
// invokes. *reconcile.Manager satisfies it.
type BackendSyncer interface {
	SyncBackend(ctx context.Context, backendName, virtualBucket string, virtualBuckets []string) (int, int, error)
}

// DashboardOps is the dashboard surface the UI reads: the aggregated snapshot
// and the lazy directory expansion behind the object browser.
// *dashboard.Aggregator satisfies it.
type DashboardOps interface {
	GetData(ctx context.Context) (*dashboard.Data, error)
	GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error)
}

// Deps holds the dependencies New requires.
type Deps struct {
	Dashboard     DashboardOps
	Sync          BackendSyncer
	Objects       *object.Manager
	Rebalancer    *worker.Rebalancer
	OverRep       *worker.OverReplicationCleaner
	AdminHandler  *admin.Handler
	DBHealthy     func() bool
	Cfg           *config.Config
	LogBuffer     *telemetry.LogBuffer
	LoginThrottle *httputil.LoginThrottle
}

// Handler serves the web UI dashboard.
type Handler struct {
	log            *slog.Logger
	dashboardOps   DashboardOps
	syncOps        BackendSyncer
	objects        *object.Manager
	rebalancer     *worker.Rebalancer
	overRep        *worker.OverReplicationCleaner
	adminHandler   *admin.Handler
	dbHealthy      func() bool
	cfg            syncutil.AtomicConfig[config.Config]
	templates      *template.Template
	logBuffer      *telemetry.LogBuffer
	loginThrottle  *httputil.LoginThrottle
	prefix         string
	adminKey       string
	adminSecret    string
	sessionKey     []byte
	forceSecure    bool
	trustedProxies []*net.IPNet
	asyncOps       asyncOpTracker
}

// New is the explicit-deps constructor. The DI layer constructs Deps and
// passes it here; tests build Deps directly. Each field is the smallest
// contract the handler uses, so wiring stays visible at the call site.
func New(d *Deps) *Handler {
	must.NotNil("d", d)
	must.NotNil("d.Objects", d.Objects)
	must.NotNil("d.AdminHandler", d.AdminHandler)
	must.NotNil("d.Cfg", d.Cfg)
	h := &Handler{
		log:            slog.Default().With(logfmt.Component("ui")),
		dashboardOps:   d.Dashboard,
		syncOps:        d.Sync,
		objects:        d.Objects,
		rebalancer:     d.Rebalancer,
		overRep:        d.OverRep,
		adminHandler:   d.AdminHandler,
		dbHealthy:      d.DBHealthy,
		templates:      loadTemplates(),
		logBuffer:      d.LogBuffer,
		loginThrottle:  d.LoginThrottle,
		adminKey:       d.Cfg.UI.AdminKey,
		adminSecret:    d.Cfg.UI.AdminSecret,
		sessionKey:     deriveSessionKey(&d.Cfg.UI),
		forceSecure:    d.Cfg.UI.ForceSecureCookies,
		trustedProxies: httputil.ParseTrustedProxies(d.Cfg.RateLimit.TrustedProxies),
	}
	h.cfg.Store(d.Cfg)
	return h
}

// deriveSessionKey produces a deterministic 32-byte HMAC key from the config
// so that sessions survive restarts and are portable across instances sharing
// the same config. session_secret is required when the UI is enabled;
// config validation rejects startup without it.
func deriveSessionKey(ui *config.UIConfig) []byte {
	mac := hmac.New(sha256.New, []byte(ui.SessionSecret))
	mac.Write([]byte("s3orch-session-key"))
	return mac.Sum(nil)
}

// UpdateConfig atomically replaces the config used by the dashboard.
// Called on SIGHUP to keep the dashboard in sync with the running config.
func (h *Handler) UpdateConfig(cfg *config.Config) {
	h.cfg.Store(cfg)
}

// clientIP extracts the real client IP from the request, respecting
// X-Forwarded-For when the peer is a trusted proxy.
func (h *Handler) clientIP(r *http.Request) string {
	return httputil.ExtractClientIP(r, h.trustedProxies)
}

// validBucketPrefix checks whether the key starts with a configured virtual bucket name.
func (h *Handler) validBucketPrefix(key string) bool {
	cfg := h.cfg.Load()
	for _, b := range cfg.Buckets {
		if strings.HasPrefix(key, b.Name+"/") {
			return true
		}
	}
	return false
}

// validBackend checks whether the backend name exists in config.
func (h *Handler) validBackend(name string) bool {
	cfg := h.cfg.Load()
	for i := range cfg.Backends {
		if cfg.Backends[i].Name == name {
			return true
		}
	}
	return false
}

// setSecurityHeaders adds security headers to dashboard responses.
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'")
}
