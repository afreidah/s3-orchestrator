// -------------------------------------------------------------------------------
// UI Handler - Session, CSRF, Login, Logout
//
// Author: Alex Freidah
//
// HMAC-signed session cookies, double-submit CSRF tokens, and the login/logout
// HTTP handlers. A login is a credential the registry resolves to a user holding
// admin-read, and the session carries that user and the access key it proved.
// requireAuth is the middleware every authenticated UI route is wrapped in: it
// re-resolves the access key on every request, so a revoked key ends the
// session, and authorizes the route against the user's current grants. HTML
// requests get redirected to the login page on auth failure, JSON requests get
// a 401.
// -------------------------------------------------------------------------------

package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// SESSION AUTH
// -------------------------------------------------------------------------

// requireAuth wraps a handler and enforces session authentication and the
// route's authorization. HTML requests are redirected to the login page; API
// requests get 401. State-changing API requests (POST) also require a valid
// CSRF token.
//
// The session is re-resolved against the live registry on every request, so a
// revoked key or a lost admin-read grant signs the user out.
func (h *Handler) requireAuth(rt *uiAPIRoute, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		user, ok := h.sessionUser(r)
		if !ok || !canUseDashboard(user) {
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
				h.log.WarnContext(r.Context(), "CSRF token mismatch", "path", r.URL.Path, "client_addr", r.RemoteAddr)
				httputil.WriteJSONError(w, http.StatusForbidden, "CSRF token missing or invalid")
				return
			}
		}

		r = r.WithContext(audit.WithWorkload(withUser(r.Context(), user), audit.WorkloadAdmin))
		if !h.authorizeRoute(w, r, rt) {
			return
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

// createSession sets an HMAC-signed session cookie and a random CSRF token
// cookie. The session names the user and the access key that proved it, so
// each later request can check the key still belongs to that user.
func (h *Handler) createSession(w http.ResponseWriter, r *http.Request, userID, accessKey string) {
	value := httputil.SignToken(h.sessionKey, userID+"|"+accessKey, time.Now().Add(sessionTTL))
	h.setSessionCookies(w, r, value, rand.Text(), int(sessionTTL.Seconds()))
}

// setSessionCookies writes the session and CSRF cookies with maxAge; a login
// passes their values and a logout passes empty ones and -1 to clear them. The
// CSRF cookie is readable by JavaScript, which sends it as X-CSRF-Token.
func (h *Handler) setSessionCookies(w http.ResponseWriter, r *http.Request, session, csrf string, maxAge int) {
	// Secure is not forced on: browsers drop a Secure cookie set over plain
	// HTTP, which would break login. forceSecure opts in; otherwise
	// IsTLSRequest infers TLS, trusting X-Forwarded-Proto only from trusted
	// proxies. A clearing write must derive the same value, or the browser
	// will not match it against the cookie it holds.
	secure := h.forceSecure || httputil.IsTLSRequest(r, h.trustedProxies)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124 / NOSONAR S2092: Secure derived from TLS detection above
		Name:     sessionCookieName,
		Value:    session,
		Path:     h.prefix + "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   maxAge,
	})
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124 / NOSONAR S2092: Secure derived from TLS detection above
		Name:     csrfCookieName,
		Value:    csrf,
		Path:     h.prefix + "/",
		HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   maxAge,
	})
}

// sessionUser resolves the request's session cookie to the user it signed in
// as. It reports false for a missing, forged or expired cookie, and for one
// whose access key no longer belongs to that user - deleted, or reissued to
// someone else - so revoking a key ends every session it opened.
func (h *Handler) sessionUser(r *http.Request) (*auth.User, bool) {
	userID, accessKey, ok := h.sessionClaims(r)
	if !ok {
		return nil, false
	}
	registry := h.d.Registry()
	if registry == nil {
		return nil, false
	}
	user, ok := registry.UserByAccessKey(accessKey)
	if !ok || user.ID != userID {
		return nil, false
	}
	return user, true
}

// sessionClaims verifies the session cookie's signature and expiry and returns
// the user and access key it names.
func (h *Handler) sessionClaims(r *http.Request) (userID, accessKey string, ok bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", "", false
	}
	payload, ok := httputil.VerifyToken(h.sessionKey, cookie.Value, time.Now())
	if !ok {
		return "", "", false
	}
	userID, accessKey, found := strings.CutLast(payload, "|")
	if !found || userID == "" || accessKey == "" {
		return "", "", false
	}
	return userID, accessKey, true
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
	if user, ok := h.sessionUser(r); ok && canUseDashboard(user) {
		http.Redirect(w, r, h.prefix+"/", http.StatusSeeOther)
		return
	}
	h.renderLoginError(w, r, http.StatusOK, "")
}

// processLoginAttempt validates the POSTed credentials, applies the
// login-attempt throttle, and either creates a session or re-renders the
// login form with the appropriate error.
func (h *Handler) processLoginAttempt(w http.ResponseWriter, r *http.Request) {
	clientIP := httputil.ExtractClientIP(r, h.trustedProxies)
	if h.d.LoginThrottle.IsLockedOut(clientIP) {
		h.log.WarnContext(r.Context(), "login attempt while locked out", "client_addr", clientIP)
		h.renderLoginError(w, r, http.StatusTooManyRequests, "Too many attempts. Try again later.")
		return
	}

	key := r.FormValue("access_key")
	secret := r.FormValue("secret_key")
	user, ok := h.resolveLogin(key, secret)
	if !ok {
		h.d.LoginThrottle.RecordFailure(clientIP)
		h.log.WarnContext(r.Context(), "failed login attempt", "client_addr", clientIP)
		h.renderLoginError(w, r, http.StatusUnauthorized, "Invalid credentials.")
		return
	}
	h.d.LoginThrottle.RecordSuccess(clientIP)

	// A valid credential without admin-read reaches the data through the S3
	// API; the dashboard is an operator console showing the whole fleet.
	if !canUseDashboard(user) {
		h.log.WarnContext(r.Context(), "login refused: credential lacks dashboard access",
			"client_addr", clientIP, "user", user.ID)
		h.renderLoginError(w, r, http.StatusForbidden, "This credential cannot use the dashboard.")
		return
	}

	h.log.InfoContext(r.Context(), "admin login", "client_addr", clientIP, "user", user.ID)
	h.createSession(w, r, user.ID, key)
	http.Redirect(w, r, h.prefix+"/", http.StatusSeeOther)
}

// resolveLogin verifies a submitted keypair against the S3 credential registry
// and reports the user it proves. Both halves are always compared, so timing
// does not reveal whether the access key or the secret was wrong.
func (h *Handler) resolveLogin(key, secret string) (*auth.User, bool) {
	registry := h.d.Registry()
	if registry == nil {
		return nil, false
	}
	user, err := registry.AuthenticateSecret(key, secret)
	if err != nil {
		return nil, false
	}
	return user, true
}

// renderLoginError writes the login form with status and an inline error
// message, which is empty on a plain GET of the page.
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
	h.setSessionCookies(w, r, "", "", -1)
	http.Redirect(w, r, h.prefix+loginPath, http.StatusSeeOther)
}
