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
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName = "s3orch_session"
	csrfCookieName    = "s3orch_csrf"
	csrfHeaderName    = "X-CSRF-Token"
	sessionTTL        = 24 * time.Hour
)

// Handler serves the web UI dashboard.
type Handler struct {
	manager        *proxy.BackendManager
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
}

// New creates a new UI handler.
func New(manager *proxy.BackendManager, dbHealthy func() bool, cfg *config.Config, logBuffer *telemetry.LogBuffer, loginThrottle *httputil.LoginThrottle) *Handler {
	h := &Handler{
		manager:        manager,
		dbHealthy:      dbHealthy,
		templates:      loadTemplates(),
		logBuffer:      logBuffer,
		loginThrottle:  loginThrottle,
		adminKey:       cfg.UI.AdminKey,
		adminSecret:    cfg.UI.AdminSecret,
		sessionKey:     deriveSessionKey(&cfg.UI),
		forceSecure:    cfg.UI.ForceSecureCookies,
		trustedProxies: httputil.ParseTrustedProxies(cfg.RateLimit.TrustedProxies),
	}
	h.cfg.Store(cfg)
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

// Register mounts the UI routes on the given mux under the configured prefix.
func (h *Handler) Register(mux *http.ServeMux, prefix string) {
	h.prefix = prefix
	mux.HandleFunc(prefix+"/login", h.handleLogin)
	mux.HandleFunc(prefix+"/logout", h.handleLogout)
	mux.HandleFunc(prefix+"/", h.requireAuth(h.handleDashboard))
	mux.HandleFunc(prefix+"/api/dashboard", h.requireAuth(h.handleAPIDashboard))
	mux.HandleFunc(prefix+"/api/tree", h.requireAuth(h.handleTreeAPI))
	mux.HandleFunc(prefix+"/api/delete", h.requireAuth(h.handleAPIDelete))
	mux.HandleFunc(prefix+"/api/delete-prefix", h.requireAuth(h.handleAPIDeletePrefix))
	mux.HandleFunc(prefix+"/api/upload", h.requireAuth(h.handleAPIUpload))
	mux.HandleFunc(prefix+"/api/download", h.requireAuth(h.handleAPIDownload))
	mux.HandleFunc(prefix+"/api/rebalance", h.requireAuth(h.handleAPIRebalance))
	mux.HandleFunc(prefix+"/api/clean-excess", h.requireAuth(h.handleAPICleanExcess))
	mux.HandleFunc(prefix+"/api/sync", h.requireAuth(h.handleAPISync))
	mux.HandleFunc(prefix+"/api/logs", h.requireAuth(h.handleAPILogs))
	mux.Handle(prefix+"/static/", http.StripPrefix(prefix+"/static/", http.FileServerFS(staticFS)))
}

// -------------------------------------------------------------------------
// JSON ERROR HELPER
// -------------------------------------------------------------------------

// writeJSONError writes a JSON error response with the correct content type.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+msg+`"}`)
}

// -------------------------------------------------------------------------
// SESSION AUTH
// -------------------------------------------------------------------------

// checkSecret compares a provided secret against the configured value.
// Supports both bcrypt hashes (prefix "$2") and plaintext comparison.
func checkSecret(configured, provided string) bool {
	if strings.HasPrefix(configured, "$2") {
		return bcrypt.CompareHashAndPassword([]byte(configured), []byte(provided)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(configured), []byte(provided)) == 1
}

// requireAuth wraps a handler and enforces session authentication.
// HTML requests are redirected to the login page; API requests get 401.
// State-changing API requests (POST) also require a valid CSRF token.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.validSession(r) {
			if strings.HasPrefix(r.URL.Path, h.prefix+"/api/") {
				slog.WarnContext(r.Context(), "UI: unauthorized API request", "path", r.URL.Path, "remote", r.RemoteAddr)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			http.Redirect(w, r, h.prefix+"/login", http.StatusSeeOther)
			return
		}

		// CSRF check on state-changing API requests
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, h.prefix+"/api/") {
			if !h.validCSRFToken(r) {
				slog.WarnContext(r.Context(), "UI: CSRF token mismatch", "path", r.URL.Path, "remote", r.RemoteAddr)
				writeJSONError(w, http.StatusForbidden, "CSRF token missing or invalid")
				return
			}
		}

		next(w, r)
	}
}

// validCSRFToken checks that the X-CSRF-Token header matches the CSRF cookie.
func (h *Handler) validCSRFToken(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	header := r.Header.Get(csrfHeaderName)
	if header == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) == 1
}

// createSession sets an HMAC-signed session cookie and a CSRF token cookie.
func (h *Handler) createSession(w http.ResponseWriter, r *http.Request, accessKey string) {
	expiry := time.Now().Add(sessionTTL).Unix()
	payload := fmt.Sprintf("%s|%d", accessKey, expiry)

	mac := hmac.New(sha256.New, h.sessionKey)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	value := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
	secure := h.forceSecure || r.TLS != nil

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     h.prefix + "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   int(sessionTTL.Seconds()),
	})

	// CSRF token: readable by JavaScript (not HttpOnly) for double-submit pattern.
	csrfToken, err := generateCSRFToken()
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    csrfToken,
		Path:     h.prefix + "/",
		HttpOnly: false, // JS must read this to send as X-CSRF-Token header
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// generateCSRFToken returns a random hex string for CSRF protection.
func generateCSRFToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand.Read failed: %w", err)
	}
	return hex.EncodeToString(b), nil
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
			slog.ErrorContext(r.Context(), "UI: failed to render login page", "error", err)
		}
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := h.clientIP(r)

	if h.loginThrottle != nil && h.loginThrottle.IsLockedOut(clientIP) {
		slog.WarnContext(r.Context(), "UI: login attempt while locked out", "remote_addr", clientIP)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		if err := h.templates.ExecuteTemplate(w, "login.html", loginPage{
			Version: telemetry.Version,
			Error:   "Too many attempts. Try again later.",
		}); err != nil {
			slog.ErrorContext(r.Context(), "UI: failed to render login page", "error", err)
		}
		return
	}

	key := r.FormValue("access_key")
	secret := r.FormValue("secret_key")

	keyMatch := subtle.ConstantTimeCompare([]byte(key), []byte(h.adminKey)) == 1
	secretMatch := checkSecret(h.adminSecret, secret)

	if !keyMatch || !secretMatch {
		if h.loginThrottle != nil {
			h.loginThrottle.RecordFailure(clientIP)
		}
		slog.WarnContext(r.Context(), "UI: failed login attempt", "remote_addr", clientIP)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		if err := h.templates.ExecuteTemplate(w, "login.html", loginPage{
			Version: telemetry.Version,
			Error:   "Invalid credentials.",
		}); err != nil {
			slog.ErrorContext(r.Context(), "UI: failed to render login page", "error", err)
		}
		return
	}

	if h.loginThrottle != nil {
		h.loginThrottle.RecordSuccess(clientIP)
	}
	slog.InfoContext(r.Context(), "UI: admin login", "remote_addr", clientIP)
	h.createSession(w, r, key)
	http.Redirect(w, r, h.prefix+"/", http.StatusSeeOther)
}

// handleLogout clears the session and CSRF cookies and redirects to login.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	secure := h.forceSecure || r.TLS != nil
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     h.prefix + "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Secure:   secure,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    "",
		Path:     h.prefix + "/",
		MaxAge:   -1,
		Secure:   secure,
	})
	http.Redirect(w, r, h.prefix+"/login", http.StatusSeeOther)
}

// -------------------------------------------------------------------------
// DASHBOARD
// -------------------------------------------------------------------------

// dashboardPage holds all data passed to the dashboard template.
type dashboardPage struct {
	Version          string
	DBHealthy        bool
	Data             *proxy.DashboardData
	Buckets          []string
	Config           configSummary
	TotalBytesUsed   int64
	TotalBytesLimit  int64
	TotalOrphanBytes int64
}

// configSummary holds non-sensitive configuration for display.
type configSummary struct {
	RoutingStrategy   string
	ReplicationFactor int
	RebalanceEnabled  bool
	RebalanceStrategy string
	RateLimitEnabled  bool
	EncryptionEnabled bool
}

// handleDashboard renders the HTML dashboard page.
func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	data, err := h.manager.GetDashboardData(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "UI: failed to get dashboard data", "error", err)
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
			data.TopLevelEntries.Entries = append(data.TopLevelEntries.Entries, store.DirEntry{
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
		},
	}

	var buf bytes.Buffer
	if err := h.templates.ExecuteTemplate(&buf, "dashboard.html", page); err != nil {
		slog.ErrorContext(r.Context(), "UI: failed to render dashboard", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// -------------------------------------------------------------------------
// JSON API
// -------------------------------------------------------------------------

// handleAPIDashboard returns dashboard data as JSON.
func (h *Handler) handleAPIDashboard(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	data, err := h.manager.GetDashboardData(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "UI: failed to get dashboard data", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to load data")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.ErrorContext(r.Context(), "UI: failed to encode dashboard JSON", "error", err)
	}
}

// handleTreeAPI returns children of a directory prefix as JSON for the
// lazy-loaded file browser.
func (h *Handler) handleTreeAPI(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	prefix := r.URL.Query().Get("prefix")
	if prefix != "" && !h.validBucketPrefix(prefix) {
		writeJSONError(w, http.StatusBadRequest, "prefix must start with a configured bucket name")
		return
	}
	startAfter := r.URL.Query().Get("startAfter")
	maxKeys := 200
	if mk := r.URL.Query().Get("maxKeys"); mk != "" {
		if parsed, err := strconv.Atoi(mk); err == nil && parsed > 0 && parsed <= 200 {
			maxKeys = parsed
		}
	}

	result, err := h.manager.GetDirectoryChildren(r.Context(), prefix, startAfter, maxKeys)
	if err != nil {
		slog.ErrorContext(r.Context(), "UI: failed to list directory children", "prefix", prefix, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to list children")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		slog.ErrorContext(r.Context(), "UI: failed to encode tree JSON", "error", err)
	}
}

// handleAPIDelete deletes a single object by key.
func (h *Handler) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Key == "" {
		writeJSONError(w, http.StatusBadRequest, "key is required")
		return
	}

	opStart := time.Now()
	defer func() {
		telemetry.RequestDuration.WithLabelValues("DELETE").Observe(time.Since(opStart).Seconds())
	}()

	if err := h.manager.ObjectManager.DeleteObject(r.Context(), req.Key); err != nil {
		slog.ErrorContext(r.Context(), "UI: failed to delete object", "key", req.Key, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "delete failed"})
		return
	}

	slog.InfoContext(r.Context(), "UI: deleted object", "key", req.Key)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleAPIDeletePrefix deletes all objects under a given key prefix.
func (h *Handler) handleAPIDeletePrefix(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req struct {
		Prefix string `json:"prefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Prefix == "" {
		writeJSONError(w, http.StatusBadRequest, "prefix is required")
		return
	}

	opStart := time.Now()
	defer func() {
		telemetry.RequestDuration.WithLabelValues("DELETE").Observe(time.Since(opStart).Seconds())
	}()

	// Collect all object keys under the prefix via pagination.
	var keys []string
	startAfter := ""
	for {
		result, err := h.manager.ObjectManager.ListObjects(r.Context(), req.Prefix, "", startAfter, 1000)
		if err != nil {
			slog.ErrorContext(r.Context(), "UI: failed to list objects for prefix delete", "prefix", req.Prefix, "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to list objects"})
			return
		}
		for i := range result.Objects {
			keys = append(keys, result.Objects[i].ObjectKey)
		}
		if !result.IsTruncated {
			break
		}
		startAfter = result.NextContinuationToken
	}

	if len(keys) == 0 {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "deleted": 0})
		return
	}

	results := h.manager.ObjectManager.DeleteObjects(r.Context(), keys)
	var errCount int
	for _, res := range results {
		if res.Err != nil {
			errCount++
		}
	}

	deleted := len(keys) - errCount
	slog.InfoContext(r.Context(), "UI: prefix delete completed", "prefix", req.Prefix, "deleted", deleted, "errors", errCount)

	w.Header().Set("Content-Type", "application/json")
	if errCount > 0 {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   fmt.Sprintf("%d of %d deletes failed", errCount, len(keys)),
			"deleted": deleted,
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "deleted": deleted})
}

// handleAPIUpload uploads a file via multipart form data.
func (h *Handler) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	const maxUploadSize = 512 << 20 // 512 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to parse form")
		return
	}

	key := r.FormValue("key")
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "key is required")
		return
	}

	if !h.validBucketPrefix(key) {
		writeJSONError(w, http.StatusBadRequest, "key must start with a configured bucket name")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		if ct := mime.TypeByExtension(filepath.Ext(header.Filename)); ct != "" {
			contentType = ct
		}
	}

	opStart := time.Now()
	defer func() {
		telemetry.RequestDuration.WithLabelValues("PUT").Observe(time.Since(opStart).Seconds())
	}()

	etag, err := h.manager.ObjectManager.PutObject(r.Context(), key, file, header.Size, contentType, nil)
	if err != nil {
		slog.ErrorContext(r.Context(), "UI: failed to upload object", "key", key, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "upload failed"})
		return
	}

	slog.InfoContext(r.Context(), "UI: uploaded object", "key", key, "size", header.Size)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "etag": etag})
}

// handleAPIDownload streams an object to the browser as a file download.
func (h *Handler) handleAPIDownload(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "key is required")
		return
	}

	if !h.validBucketPrefix(key) {
		writeJSONError(w, http.StatusBadRequest, "key must start with a configured bucket name")
		return
	}

	result, err := h.manager.ObjectManager.GetObject(r.Context(), key, "")
	if err != nil {
		if errors.Is(err, store.ErrObjectNotFound) {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		slog.ErrorContext(r.Context(), "UI: failed to download object", "key", key, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "download failed")
		return
	}
	defer result.Body.Close()

	slog.InfoContext(r.Context(), "UI: downloaded object", "key", key, "size", result.Size)

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(key)))
	w.Header().Set("Content-Type", "application/octet-stream")
	if result.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	}

	_, _ = bufpool.Copy(w, result.Body)
}

// handleAPIRebalance triggers an on-demand rebalance across backends.
func (h *Handler) handleAPIRebalance(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	rebalCfg := h.manager.Rebalancer.Config()
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

	moved, err := h.manager.Rebalancer.Rebalance(r.Context(), runCfg)
	if err != nil {
		slog.ErrorContext(r.Context(), "UI: rebalance failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "rebalance failed"})
		return
	}

	slog.InfoContext(r.Context(), "UI: manual rebalance completed", "moved", moved)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "moved": moved})
}

// handleAPICleanExcess triggers an on-demand over-replication cleanup.
func (h *Handler) handleAPICleanExcess(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	rcfg := h.manager.OverReplicationCleaner.Config()
	if rcfg == nil {
		rcfg = &h.cfg.Load().Replication
	}
	if rcfg.Factor <= 1 {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "removed": 0, "reason": "replication factor <= 1"})
		return
	}

	cfg := *rcfg
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 5
	}

	removed, err := h.manager.OverReplicationCleaner.Clean(r.Context(), cfg)
	if err != nil {
		slog.ErrorContext(r.Context(), "UI: over-replication cleanup failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "cleanup failed"})
		return
	}

	slog.InfoContext(r.Context(), "UI: manual over-replication cleanup completed", "removed", removed)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "removed": removed})
}

// handleAPISync triggers a backend sync to import pre-existing objects.
func (h *Handler) handleAPISync(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req struct {
		Backend string `json:"backend"`
		Bucket  string `json:"bucket"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Backend == "" || req.Bucket == "" {
		writeJSONError(w, http.StatusBadRequest, "backend and bucket are required")
		return
	}

	if !h.validBackend(req.Backend) {
		writeJSONError(w, http.StatusBadRequest, "invalid backend or bucket")
		return
	}

	if !h.validBucketPrefix(req.Bucket + "/") {
		writeJSONError(w, http.StatusBadRequest, "invalid backend or bucket")
		return
	}

	cfg := h.cfg.Load()
	bucketNames := make([]string, len(cfg.Buckets))
	for i, b := range cfg.Buckets {
		bucketNames[i] = b.Name
	}

	imported, skipped, err := h.manager.SyncBackend(r.Context(), req.Backend, req.Bucket, bucketNames)
	if err != nil {
		slog.ErrorContext(r.Context(), "UI: sync failed", "backend", req.Backend, "bucket", req.Bucket, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "sync failed"})
		return
	}

	slog.InfoContext(r.Context(), "UI: manual sync completed", "backend", req.Backend, "bucket", req.Bucket,
		"imported", imported, "skipped", skipped)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "imported": imported, "skipped": skipped})
}

// -------------------------------------------------------------------------
// LOGS API
// -------------------------------------------------------------------------

// logsResponse wraps log entries with pagination metadata.
type logsResponse struct {
	Entries []telemetry.LogEntry `json:"entries"`
	HasMore bool                 `json:"hasMore"`
}

// handleAPILogs returns buffered log entries as JSON. Supports query
// parameters for filtering: level (minimum severity), since (RFC3339
// timestamp), before (RFC3339 timestamp for pagination), component,
// and limit.
func (h *Handler) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)

	opts := telemetry.LogQueryOpts{}

	if lvl := r.URL.Query().Get("level"); lvl != "" {
		switch strings.ToUpper(lvl) {
		case "DEBUG":
			opts.MinLevel = slog.LevelDebug
		case "INFO":
			opts.MinLevel = slog.LevelInfo
		case "WARN":
			opts.MinLevel = slog.LevelWarn
		case "ERROR":
			opts.MinLevel = slog.LevelError
		}
	}

	if since := r.URL.Query().Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			opts.Since = t
		}
	}

	if before := r.URL.Query().Get("before"); before != "" {
		if t, err := time.Parse(time.RFC3339, before); err == nil {
			opts.Before = t
		}
	}

	requestedLimit := 0
	if limit := r.URL.Query().Get("limit"); limit != "" {
		if n, err := strconv.Atoi(limit); err == nil && n > 0 {
			requestedLimit = n
		}
	}

	// Over-fetch by 1 to detect whether more entries exist.
	if requestedLimit > 0 {
		opts.Limit = requestedLimit + 1
	}

	opts.Component = r.URL.Query().Get("component")

	entries := h.logBuffer.Entries(&opts)
	if entries == nil {
		entries = []telemetry.LogEntry{}
	}

	resp := logsResponse{Entries: entries}
	if requestedLimit > 0 && len(entries) > requestedLimit {
		resp.Entries = entries[:requestedLimit]
		resp.HasMore = true
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.ErrorContext(r.Context(), "UI: failed to encode logs JSON", "error", err)
	}
}
