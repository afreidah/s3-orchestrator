// -------------------------------------------------------------------------------
// UI Handler - Built-in Web Dashboard
//
// Author: Alex Freidah
//
// HTTP handler for the built-in operational dashboard. Renders a server-side
// HTML page showing backend quota stats, monthly usage, and configuration
// summary. Also provides a JSON API endpoint for programmatic access.
// All routes (except static assets) are gated behind session authentication
// using HMAC-signed cookies.
// -------------------------------------------------------------------------------

// Package ui provides the built-in web dashboard for operational visibility,
// serving HTML pages, JSON API endpoints, and static assets.
package ui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/storage"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

const (
	sessionCookieName = "s3orch_session"
	sessionTTL        = 24 * time.Hour
)

// Handler serves the web UI dashboard.
type Handler struct {
	manager     *storage.BackendManager
	dbHealthy   func() bool
	cfg         atomic.Pointer[config.Config]
	templates   *template.Template
	prefix      string
	adminKey    string
	adminSecret string
	sessionKey  []byte
}

// New creates a new UI handler.
func New(manager *storage.BackendManager, dbHealthy func() bool, cfg *config.Config) *Handler {
	sessionKey := make([]byte, 32)
	if _, err := rand.Read(sessionKey); err != nil {
		panic("failed to generate session key: " + err.Error())
	}

	h := &Handler{
		manager:     manager,
		dbHealthy:   dbHealthy,
		templates:   loadTemplates(),
		adminKey:    cfg.UI.AdminKey,
		adminSecret: cfg.UI.AdminSecret,
		sessionKey:  sessionKey,
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
	h.prefix = prefix
	mux.HandleFunc(prefix+"/login", h.handleLogin)
	mux.HandleFunc(prefix+"/logout", h.handleLogout)
	mux.HandleFunc(prefix+"/", h.requireAuth(h.handleDashboard))
	mux.HandleFunc(prefix+"/api/dashboard", h.requireAuth(h.handleAPIDashboard))
	mux.HandleFunc(prefix+"/api/tree", h.requireAuth(h.handleTreeAPI))
	mux.HandleFunc(prefix+"/api/delete", h.requireAuth(h.handleAPIDelete))
	mux.HandleFunc(prefix+"/api/upload", h.requireAuth(h.handleAPIUpload))
	mux.HandleFunc(prefix+"/api/rebalance", h.requireAuth(h.handleAPIRebalance))
	mux.HandleFunc(prefix+"/api/sync", h.requireAuth(h.handleAPISync))
	mux.Handle(prefix+"/static/", http.StripPrefix(prefix+"/static/", http.FileServerFS(staticFS)))
}

// -------------------------------------------------------------------------
// SESSION AUTH
// -------------------------------------------------------------------------

// requireAuth wraps a handler and enforces session authentication.
// HTML requests are redirected to the login page; API requests get 401.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.validSession(r) {
			next(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, h.prefix+"/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		http.Redirect(w, r, h.prefix+"/login", http.StatusSeeOther)
	}
}

// createSession sets an HMAC-signed session cookie on the response.
func (h *Handler) createSession(w http.ResponseWriter, r *http.Request, accessKey string) {
	expiry := time.Now().Add(sessionTTL).Unix()
	payload := fmt.Sprintf("%s|%d", accessKey, expiry)

	mac := hmac.New(sha256.New, h.sessionKey)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	value := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     h.prefix + "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// validSession checks whether the request carries a valid, non-expired session cookie.
func (h *Handler) validSession(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}

	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	payload := string(payloadBytes)

	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, h.sessionKey)
	mac.Write([]byte(payload))
	if !hmac.Equal(mac.Sum(nil), sig) {
		return false
	}

	pipeIdx := strings.LastIndex(payload, "|")
	if pipeIdx < 0 {
		return false
	}
	expiry, err := strconv.ParseInt(payload[pipeIdx+1:], 10, 64)
	if err != nil {
		return false
	}

	return time.Now().Unix() < expiry
}

// loginPage holds data for the login template.
type loginPage struct {
	Version string
	Error   string
}

// handleLogin serves the login page (GET) and processes login attempts (POST).
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method == http.MethodGet {
		if h.validSession(r) {
			http.Redirect(w, r, h.prefix+"/", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := h.templates.ExecuteTemplate(w, "login.html", loginPage{Version: telemetry.Version}); err != nil {
			slog.Error("UI: failed to render login page", "error", err)
		}
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.FormValue("access_key")
	secret := r.FormValue("secret_key")

	keyMatch := subtle.ConstantTimeCompare([]byte(key), []byte(h.adminKey)) == 1
	secretMatch := subtle.ConstantTimeCompare([]byte(secret), []byte(h.adminSecret)) == 1

	if !keyMatch || !secretMatch {
		slog.Warn("UI: failed login attempt", "remote_addr", r.RemoteAddr)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		if err := h.templates.ExecuteTemplate(w, "login.html", loginPage{
			Version: telemetry.Version,
			Error:   "Invalid credentials.",
		}); err != nil {
			slog.Error("UI: failed to render login page", "error", err)
		}
		return
	}

	slog.Info("UI: admin login", "remote_addr", r.RemoteAddr)
	h.createSession(w, r, key)
	http.Redirect(w, r, h.prefix+"/", http.StatusSeeOther)
}

// handleLogout clears the session cookie and redirects to login.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     h.prefix + "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Secure:   true,
	})
	http.Redirect(w, r, h.prefix+"/login", http.StatusSeeOther)
}

// -------------------------------------------------------------------------
// DASHBOARD
// -------------------------------------------------------------------------

// dashboardPage holds all data passed to the dashboard template.
type dashboardPage struct {
	Version         string
	DBHealthy       bool
	Data            *storage.DashboardData
	Buckets         []string
	Config          configSummary
	TotalBytesUsed  int64
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

	// Ensure every configured bucket appears as a top-level directory in the
	// object tree, even when the bucket has no files yet.
	existing := make(map[string]bool, len(data.TopLevelEntries.Entries))
	for _, e := range data.TopLevelEntries.Entries {
		existing[e.Name] = true
	}
	for _, name := range bucketNames {
		dirName := name + "/"
		if !existing[dirName] {
			data.TopLevelEntries.Entries = append(data.TopLevelEntries.Entries, storage.DirEntry{
				Name:  dirName,
				IsDir: true,
			})
		}
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

// -------------------------------------------------------------------------
// JSON API
// -------------------------------------------------------------------------

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

// handleAPIDelete deletes a single object by key.
func (h *Handler) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		http.Error(w, `{"error":"key is required"}`, http.StatusBadRequest)
		return
	}

	if err := h.manager.DeleteObject(r.Context(), req.Key); err != nil {
		slog.Error("UI: failed to delete object", "key", req.Key, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	slog.Info("UI: deleted object", "key", req.Key)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleAPIUpload uploads a file via multipart form data.
func (h *Handler) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	const maxUploadSize = 512 << 20 // 512 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, `{"error":"failed to parse form"}`, http.StatusBadRequest)
		return
	}

	key := r.FormValue("key")
	if key == "" {
		http.Error(w, `{"error":"key is required"}`, http.StatusBadRequest)
		return
	}

	// Validate the key starts with a configured virtual bucket name.
	cfg := h.cfg.Load()
	validBucket := false
	for _, b := range cfg.Buckets {
		if strings.HasPrefix(key, b.Name+"/") {
			validBucket = true
			break
		}
	}
	if !validBucket {
		http.Error(w, `{"error":"key must start with a configured bucket name"}`, http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file is required"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		if ct := mime.TypeByExtension(filepath.Ext(header.Filename)); ct != "" {
			contentType = ct
		}
	}

	etag, err := h.manager.PutObject(r.Context(), key, file, header.Size, contentType)
	if err != nil {
		slog.Error("UI: failed to upload object", "key", key, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	slog.Info("UI: uploaded object", "key", key, "size", header.Size)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "etag": etag})
}

// handleAPIRebalance triggers an on-demand rebalance across backends.
func (h *Handler) handleAPIRebalance(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	rebalCfg := h.manager.RebalanceConfig()
	if rebalCfg == nil {
		rebalCfg = &h.cfg.Load().Rebalance
	}
	// Ensure required fields have sensible defaults for on-demand runs.
	runCfg := *rebalCfg
	if runCfg.Strategy == "" {
		runCfg.Strategy = "spread"
	}
	if runCfg.BatchSize == 0 {
		runCfg.BatchSize = 100
	}
	if runCfg.Threshold == 0 {
		runCfg.Threshold = 0.1
	}
	if runCfg.Concurrency == 0 {
		runCfg.Concurrency = 5
	}

	moved, err := h.manager.Rebalance(r.Context(), runCfg)
	if err != nil {
		slog.Error("UI: rebalance failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	slog.Info("UI: manual rebalance completed", "moved", moved)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "moved": moved})
}

// handleAPISync triggers a backend sync to import pre-existing objects.
func (h *Handler) handleAPISync(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Backend string `json:"backend"`
		Bucket  string `json:"bucket"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.Backend == "" || req.Bucket == "" {
		http.Error(w, `{"error":"backend and bucket are required"}`, http.StatusBadRequest)
		return
	}

	cfg := h.cfg.Load()

	// Validate backend name exists in config.
	validBackend := false
	for i := range cfg.Backends {
		if cfg.Backends[i].Name == req.Backend {
			validBackend = true
			break
		}
	}
	if !validBackend {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown backend: " + req.Backend})
		return
	}

	// Validate bucket name is a configured virtual bucket.
	validBucket := false
	for _, b := range cfg.Buckets {
		if b.Name == req.Bucket {
			validBucket = true
			break
		}
	}
	if !validBucket {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown bucket: " + req.Bucket})
		return
	}

	bucketNames := make([]string, len(cfg.Buckets))
	for i, b := range cfg.Buckets {
		bucketNames[i] = b.Name
	}

	imported, skipped, err := h.manager.SyncBackend(r.Context(), req.Backend, req.Bucket, bucketNames)
	if err != nil {
		slog.Error("UI: sync failed", "backend", req.Backend, "bucket", req.Bucket, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	slog.Info("UI: manual sync completed", "backend", req.Backend, "bucket", req.Bucket,
		"imported", imported, "skipped", skipped)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "imported": imported, "skipped": skipped})
}
