// -------------------------------------------------------------------------------
// CORS Policy - Preflight Answering and Response Decoration
//
// Author: Alex Freidah
//
// Middleware that answers browser preflight requests from the compiled rule
// set and attaches the access-control headers to the cross-origin responses
// that follow. Sits directly in front of the S3 handler, inside the rate
// limiter and admission control, so an unauthenticated preflight is bounded
// by the same protections as any other request while never reaching the
// authentication it cannot satisfy.
// -------------------------------------------------------------------------------

package cors

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// Request and response header names, and the outcome labels the preflight
// counter carries.
const (
	headerOrigin         = "Origin"
	headerRequestMethod  = "Access-Control-Request-Method"
	headerRequestHeaders = "Access-Control-Request-Headers"
	headerAllowOrigin    = "Access-Control-Allow-Origin"
	headerAllowMethods   = "Access-Control-Allow-Methods"
	headerAllowHeaders   = "Access-Control-Allow-Headers"
	headerExposeHeaders  = "Access-Control-Expose-Headers"
	headerMaxAge         = "Access-Control-Max-Age"
	headerVary           = "Vary"

	resultAllowed  = "allowed"
	resultRejected = "rejected"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// BucketResolver extracts the virtual bucket a request path addresses. A
// preflight carries no credentials, so the bucket cannot come from the
// credential the way it does for an authenticated request; the composition
// layer supplies the S3 transport's own path convention rather than this
// package growing a second copy of it.
type BucketResolver func(path string) (bucket string, ok bool)

// Policy answers preflights and decorates cross-origin responses from the
// rule set most recently stored on it.
//
// The rules live behind an atomic pointer so a config reload replaces them
// wholesale between requests, matching how the bucket credential registry is
// published. A nil rule set refuses every preflight, which is what an
// instance whose reload failed should do.
type Policy struct {
	rules    syncutil.AtomicConfig[Registry]
	resolve  BucketResolver
	writeErr httputil.ErrorWriter
	log      *slog.Logger
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// New creates a Policy with no rules. The caller stores the compiled set via
// SetRules before serving.
func New(resolve BucketResolver, writeErr httputil.ErrorWriter) *Policy {
	must.NotNil("resolve", resolve)
	must.NotNil("writeErr", writeErr)
	return &Policy{
		resolve:  resolve,
		writeErr: writeErr,
		log:      slog.Default().With(logfmt.Component("cors")),
	}
}

// SetRules atomically replaces the rule set. Safe to call concurrently with
// request handling.
func (p *Policy) SetRules(reg *Registry) {
	p.rules.Store(reg)
}

// -------------------------------------------------------------------------
// MIDDLEWARE
// -------------------------------------------------------------------------

// Middleware wraps next with preflight answering and response decoration.
//
// A request without an Origin header is not cross-origin and passes straight
// through, which is every request from a server-side client.
func (p *Policy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get(headerOrigin)
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodOptions && r.Header.Get(headerRequestMethod) != "" {
			p.servePreflight(w, r, origin)
			return
		}
		p.decorate(w, r, origin)
		next.ServeHTTP(w, r)
	})
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// servePreflight answers the browser's preflight without consulting
// authentication, which a preflight cannot carry. The answer grants nothing
// on its own: it reports whether the request the browser intends to send is
// one the operator allows, and that request authenticates normally.
func (p *Policy) servePreflight(w http.ResponseWriter, r *http.Request, origin string) {
	w.Header().Add(headerVary, strings.Join([]string{headerOrigin, headerRequestMethod, headerRequestHeaders}, ", "))

	matched := p.matchPreflight(r, origin)
	if matched == nil {
		p.rejectPreflight(w, r, origin)
		return
	}

	w.Header().Set(headerAllowOrigin, origin)
	w.Header().Set(headerAllowMethods, matched.allowMethods)
	if reqHeaders := r.Header.Get(headerRequestHeaders); reqHeaders != "" {
		w.Header().Set(headerAllowHeaders, reqHeaders)
	}
	if matched.maxAge != "" {
		w.Header().Set(headerMaxAge, matched.maxAge)
	}
	telemetry.CORSPreflightTotal.WithLabelValues(resultAllowed).Inc()
	w.WriteHeader(http.StatusOK)
}

// matchPreflight resolves the bucket and finds the rule admitting the
// announced method and headers, or nil when either step fails.
func (p *Policy) matchPreflight(r *http.Request, origin string) *rule {
	reg := p.rules.Load()
	if reg == nil {
		return nil
	}
	bucket, ok := p.resolve(r.URL.Path)
	if !ok {
		return nil
	}
	method := strings.ToUpper(strings.TrimSpace(r.Header.Get(headerRequestMethod)))
	return reg.matchPreflight(bucket, origin, method, parseHeaderList(r.Header.Get(headerRequestHeaders)))
}

// rejectPreflight refuses the preflight with no access-control headers.
//
// The response is identical whether the bucket exists, has no rules, or has
// rules that do not admit the request, so a preflight cannot be used to
// enumerate buckets from a browser - the one caller that can reach this
// endpoint without a credential.
func (p *Policy) rejectPreflight(w http.ResponseWriter, r *http.Request, origin string) {
	telemetry.CORSPreflightTotal.WithLabelValues(resultRejected).Inc()
	p.log.DebugContext(r.Context(), "cors preflight refused",
		"origin", origin,
		"path", r.URL.Path,
		"requested_method", r.Header.Get(headerRequestMethod),
	)
	p.writeErr(w, http.StatusForbidden, "AccessForbidden", "CORS preflight refused")
}

// decorate adds the access-control headers to a cross-origin request that is
// not a preflight, before the handler writes its response.
//
// A request no rule admits is passed through undecorated rather than
// refused. The Origin header is not proof of a browser, and a signed request
// from a non-browser client that happens to carry one is not subject to CORS
// at all; a browser, meanwhile, is already blocked from reading a response
// that carries no allow header.
func (p *Policy) decorate(w http.ResponseWriter, r *http.Request, origin string) {
	w.Header().Add(headerVary, headerOrigin)

	reg := p.rules.Load()
	if reg == nil {
		return
	}
	bucket, ok := p.resolve(r.URL.Path)
	if !ok {
		return
	}
	matched := reg.matchActual(bucket, origin, r.Method)
	if matched == nil {
		return
	}

	w.Header().Set(headerAllowOrigin, origin)
	if matched.exposeHeaders != "" {
		w.Header().Set(headerExposeHeaders, matched.exposeHeaders)
	}
}

// parseHeaderList splits an Access-Control-Request-Headers value into header
// names. Returns nil for an absent or empty header, which is the browser
// saying it intends to send nothing beyond the safelisted headers.
//
// Case is left as the browser sent it; the matcher folds it.
func parseHeaderList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if name := strings.TrimSpace(part); name != "" {
			out = append(out, name)
		}
	}
	return out
}
