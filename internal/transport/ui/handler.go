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
//   - async.go          asyncOpTracker (shared by admin_ops and admin_actions)
//   - templates.go      template loading + embedded static FS
// -------------------------------------------------------------------------------

package ui

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// Session and CSRF cookie names, plus the header, path and message literals
// the handlers share.
const (
	sessionCookieName = "s3orch_session"
	csrfCookieName    = "s3orch_csrf"
	csrfHeaderName    = "X-CSRF-Token"
	sessionTTL        = 24 * time.Hour

	contentTypeHTML      = "text/html; charset=utf-8"
	loginPath            = "/login"
	errLoginRenderFailed = "failed to render login page"
)

// -------------------------------------------------------------------------
// INTERFACES
// -------------------------------------------------------------------------

// BackendSyncer is the backend-sync surface the UI's admin actions pane
// invokes. *reconcile.Manager satisfies it.
type BackendSyncer interface {
	SyncBackend(ctx context.Context, backendName, prefix string, knownBuckets []string) (int, int, error)
}

// DashboardOps is the dashboard surface the UI reads: the aggregated snapshot
// and the lazy directory expansion behind the object browser.
// *dashboard.Aggregator satisfies it.
type DashboardOps interface {
	GetData(ctx context.Context) (*dashboard.Data, error)
	GetDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error)
}

// DeclaredBuckets is the virtual bucket set the UI resolves a browsed key
// against, covering both the config file and the store.
// *provisioning.Declared satisfies it.
type DeclaredBuckets interface {
	HasPrefix(key string) bool
	Names() []string
}

// Deps holds the dependencies New requires.
type Deps struct {
	Dashboard     DashboardOps
	Sync          BackendSyncer
	Ops           *ops.Services
	DBHealthy     func() bool
	Buckets       DeclaredBuckets
	Config        *config.Live
	LogBuffer     admin.LogReader
	LoginThrottle *httputil.LoginThrottle
	Registry      func() *auth.BucketRegistry
}

// Handler serves the web UI dashboard. d holds the dependencies New was given;
// the remaining fields are derived from them or are the handler's own state.
type Handler struct {
	d              Deps
	log            *slog.Logger
	templates      *template.Template
	logs           http.HandlerFunc
	prefix         string
	sessionKey     []byte
	forceSecure    bool
	trustedProxies []*net.IPNet
	asyncOps       asyncOpTracker
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// New is the explicit-deps constructor. The DI layer constructs Deps and
// passes it here; tests build Deps directly. Each field is the smallest
// contract the handler uses, so wiring stays visible at the call site.
func New(d *Deps) *Handler {
	must.NotNil("d", d)
	must.NotNil("d.Ops", d.Ops)
	must.NotNil("d.Ops.Objects", d.Ops.Objects)
	must.NotNil("d.Ops.Integrity", d.Ops.Integrity)
	must.NotNil("d.Ops.Replication", d.Ops.Replication)
	must.NotNil("d.Ops.Rebalance", d.Ops.Rebalance)
	must.NotNil("d.Ops.Encryption", d.Ops.Encryption)
	must.NotNil("d.Ops.Compression", d.Ops.Compression)
	must.NotNil("d.Buckets", d.Buckets)
	must.NotNil("d.Config", d.Config)
	must.NotNil("d.LoginThrottle", d.LoginThrottle)
	must.NotNil("d.Registry", d.Registry)
	cfg := d.Config.Load()
	return &Handler{
		d:              *d,
		log:            logfmt.Logger("ui"),
		templates:      loadTemplates(),
		logs:           admin.LogsHandler(d.LogBuffer),
		sessionKey:     deriveSessionKey(&cfg.UI),
		forceSecure:    cfg.UI.ForceSecureCookies,
		trustedProxies: httputil.ParseTrustedProxies(cfg.RateLimit.TrustedProxies),
	}
}

// deriveSessionKey derives the session HMAC key from session_secret, so sessions
// survive restarts and work across instances.
func deriveSessionKey(ui *config.UIConfig) []byte {
	mac := hmac.New(sha256.New, []byte(ui.SessionSecret))
	mac.Write([]byte("s3orch-session-key"))
	return mac.Sum(nil)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// setSecurityHeaders adds security headers to dashboard responses.
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'")
}
