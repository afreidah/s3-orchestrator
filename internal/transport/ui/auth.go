// -------------------------------------------------------------------------------
// UI Handler - Session, CSRF, Login, Logout
//
// Author: Alex Freidah
//
// HMAC-signed session cookies, double-submit CSRF tokens, the bcrypt/plain
// secret comparator, and the login/logout HTTP handlers. requireAuth is
// the middleware every authenticated UI route is wrapped in; HTML
// requests get redirected to the login page on auth failure, JSON
// requests get a 401.
// -------------------------------------------------------------------------------

package ui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

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
				h.log.WarnContext(r.Context(), "unauthorized API request", "path", r.URL.Path, "client_addr", r.RemoteAddr)
				httputil.WriteJSONError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			http.Redirect(w, r, h.prefix+loginPath, http.StatusSeeOther)
			return
		}

		// CSRF check on state-changing API requests
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, h.prefix+"/api/") {
			if !h.validCSRFToken(r) {
				h.log.WarnContext(r.Context(), "cSRF token mismatch", "path", r.URL.Path, "client_addr", r.RemoteAddr)
				httputil.WriteJSONError(w, http.StatusForbidden, "CSRF token missing or invalid")
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
	// Secure must remain dynamic: forcing true unconditionally makes
	// browsers silently drop the cookie when the request arrived over
	// plain HTTP (untrusted reverse proxy, local dev), which would
	// break login. forceSecure lets operators opt in when the
	// deployment guarantees TLS; otherwise IsTLSRequest infers it
	// from a trusted X-Forwarded-Proto.
	secure := h.forceSecure || httputil.IsTLSRequest(r, h.trustedProxies)

	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124 / NOSONAR S2092: Secure derived from TLS detection above
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
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124 / NOSONAR S2092: Secure derived from TLS detection above
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

// -------------------------------------------------------------------------
// LOGIN / LOGOUT
// -------------------------------------------------------------------------

// loginPage holds data for the login template.
type loginPage struct {
	Version string
	Error   string
}

// handleLogin serves the login page (GET) and processes login attempts
// (POST).
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	switch r.Method {
	case http.MethodGet:
		h.serveLoginPage(w, r)
	case http.MethodPost:
		h.processLoginAttempt(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveLoginPage renders the GET /login page; redirects to the dashboard
// when the request already carries a valid session.
func (h *Handler) serveLoginPage(w http.ResponseWriter, r *http.Request) {
	if h.validSession(r) {
		http.Redirect(w, r, h.prefix+"/", http.StatusSeeOther)
		return
	}
	w.Header().Set(headerContentType, contentTypeHTML)
	if err := h.templates.ExecuteTemplate(w, "login.html", loginPage{Version: telemetry.Version}); err != nil {
		h.log.ErrorContext(r.Context(), errLoginRenderFailed, "error", err)
	}
}

// processLoginAttempt validates the POSTed credentials, applies the
// login-attempt throttle, and either creates a session or re-renders the
// login form with the appropriate error.
func (h *Handler) processLoginAttempt(w http.ResponseWriter, r *http.Request) {
	clientIP := h.clientIP(r)
	if h.loginThrottle != nil && h.loginThrottle.IsLockedOut(clientIP) {
		h.log.WarnContext(r.Context(), "login attempt while locked out", "client_addr", clientIP)
		h.renderLoginError(w, r, http.StatusTooManyRequests, "Too many attempts. Try again later.")
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
		h.log.WarnContext(r.Context(), "failed login attempt", "client_addr", clientIP)
		h.renderLoginError(w, r, http.StatusUnauthorized, "Invalid credentials.")
		return
	}

	if h.loginThrottle != nil {
		h.loginThrottle.RecordSuccess(clientIP)
	}
	h.log.InfoContext(r.Context(), "admin login", "client_addr", clientIP)
	h.createSession(w, r, key)
	http.Redirect(w, r, h.prefix+"/", http.StatusSeeOther)
}

// renderLoginError writes the login form with status and an inline error
// message. Used by both the throttle-lockout and bad-credential paths.
func (h *Handler) renderLoginError(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	w.Header().Set(headerContentType, contentTypeHTML)
	w.WriteHeader(status)
	if err := h.templates.ExecuteTemplate(w, "login.html", loginPage{
		Version: telemetry.Version,
		Error:   errMsg,
	}); err != nil {
		h.log.ErrorContext(r.Context(), errLoginRenderFailed, "error", err)
	}
}

// handleLogout clears the session and CSRF cookies and redirects to login.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Secure must remain dynamic; see createSession for the rationale.
	// The clear-cookie write must use the same Secure value the original
	// SetCookie used, otherwise the browser will not match the cookie
	// against the deletion request and will leave the original in place.
	secure := h.forceSecure || httputil.IsTLSRequest(r, h.trustedProxies)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124 / NOSONAR S2092: Secure derived from TLS detection above
		Name:     sessionCookieName,
		Value:    "",
		Path:     h.prefix + "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Secure:   secure,
	})
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124 / NOSONAR S2092: Secure derived from TLS detection above
		Name:   csrfCookieName,
		Value:  "",
		Path:   h.prefix + "/",
		MaxAge: -1,
		Secure: secure,
	})
	http.Redirect(w, r, h.prefix+loginPath, http.StatusSeeOther)
}
