// -------------------------------------------------------------------------------
// CORS Middleware Tests
//
// Author: Alex Freidah
//
// Covers the three paths through the middleware: a preflight answered from
// the rules, a preflight refused, and a cross-origin request decorated on its
// way to the handler. The refusal cases pin that the response carries no
// access-control headers and reads identically whether or not the bucket
// exists, which is what keeps an unauthenticated caller from enumerating
// buckets through it.
// -------------------------------------------------------------------------------

package cors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// testBuckets is the rule set every case here matches against: one bucket
// allowing an app origin, and a second whose rules admit any origin.
func testBuckets() []config.BucketConfig {
	return []config.BucketConfig{
		{Name: "photos", CORS: []config.CORSRule{{
			AllowedOrigins: []string{"https://app.example.com"},
			AllowedMethods: []string{"GET", "PUT"},
			AllowedHeaders: []string{"content-type", "x-amz-*"},
			ExposeHeaders:  []string{"ETag"},
			MaxAge:         3600,
		}}},
		{Name: "public", CORS: []config.CORSRule{{
			AllowedOrigins: []string{"*"},
			AllowedMethods: []string{"GET"},
		}}},
		{Name: "private"},
	}
}

// newTestPolicy builds a Policy over testBuckets with the S3 path convention
// and an error writer that records nothing beyond the status.
func newTestPolicy(t *testing.T) *Policy {
	t.Helper()
	reg, err := NewRegistry(testBuckets())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	p := New(bucketFromPath, writeTestError)
	p.SetRules(reg)
	return p
}

// bucketFromPath is the same first-segment convention the S3 transport uses,
// kept local so the test does not import it.
func bucketFromPath(path string) (string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", false
	}
	bucket, _, _ := strings.Cut(trimmed, "/")
	if bucket == "" {
		return "", false
	}
	return bucket, true
}

// writeTestError stands in for the S3-XML error writer.
func writeTestError(w http.ResponseWriter, status int, errCode, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte("<Error><Code>" + errCode + "</Code><Message>" + message + "</Message></Error>"))
}

// serve runs the request through the middleware, recording whether the
// wrapped handler was reached.
func serve(p *Policy, r *http.Request) (*httptest.ResponseRecorder, bool) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	p.Middleware(next).ServeHTTP(rec, r)
	return rec, reached
}

// preflight builds a preflight request for the given path, origin and
// intended method.
func preflight(path, origin, method string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, path, nil)
	r.Header.Set("Origin", origin)
	r.Header.Set("Access-Control-Request-Method", method)
	return r
}

// -------------------------------------------------------------------------
// PREFLIGHT
// -------------------------------------------------------------------------

// TestMiddleware_PreflightAllowed verifies an allowed preflight is answered
// by the middleware itself, never reaching the handler that would reject it
// for carrying no credentials.
func TestMiddleware_PreflightAllowed(t *testing.T) {
	t.Parallel()
	p := newTestPolicy(t)
	r := preflight("/photos/doc.pdf", "https://app.example.com", "PUT")
	r.Header.Set("Access-Control-Request-Headers", "content-type, x-amz-date")

	rec, reached := serve(p, r)

	if reached {
		t.Error("preflight reached the wrapped handler, want it answered by the middleware")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Allow-Origin = %q, want the request origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "GET, PUT" {
		t.Errorf("Allow-Methods = %q, want %q", got, "GET, PUT")
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "content-type, x-amz-date" {
		t.Errorf("Allow-Headers = %q, want the announced headers echoed", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "3600" {
		t.Errorf("Max-Age = %q, want %q", got, "3600")
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q, want it to include Origin", got)
	}
}

// TestMiddleware_PreflightRefusals covers every way a preflight fails and
// pins that all of them look the same from outside.
func TestMiddleware_PreflightRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		path    string
		origin  string
		method  string
		headers string
	}{
		{"origin not allowed", "/photos/doc.pdf", "https://evil.test", "PUT", ""},
		{"method not allowed", "/photos/doc.pdf", "https://app.example.com", "DELETE", ""},
		{"header not allowed", "/photos/doc.pdf", "https://app.example.com", "PUT", "x-custom"},
		{"bucket has no rules", "/private/doc.pdf", "https://app.example.com", "GET", ""},
		{"bucket does not exist", "/nonexistent/doc.pdf", "https://app.example.com", "GET", ""},
		{"path names no bucket", "/", "https://app.example.com", "GET", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newTestPolicy(t)
			r := preflight(tc.path, tc.origin, tc.method)
			if tc.headers != "" {
				r.Header.Set("Access-Control-Request-Headers", tc.headers)
			}

			rec, reached := serve(p, r)

			assertRefused(t, rec, reached)
		})
	}
}

// assertRefused checks the shape every refusal shares: the handler is never
// reached, the status is 403, and no access-control header leaks onto the
// response.
func assertRefused(t *testing.T, rec *httptest.ResponseRecorder, reached bool) {
	t.Helper()
	if reached {
		t.Error("refused preflight reached the wrapped handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want it absent on a refusal", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "" {
		t.Errorf("Allow-Methods = %q, want it absent on a refusal", got)
	}
}

// TestMiddleware_PreflightRefusalHidesBucketExistence pins that a bucket with
// no rules and a bucket that does not exist produce byte-identical
// responses, so the refusal cannot be used to probe which buckets are
// configured.
func TestMiddleware_PreflightRefusalHidesBucketExistence(t *testing.T) {
	t.Parallel()
	p := newTestPolicy(t)

	existing, _ := serve(p, preflight("/private/doc.pdf", "https://app.example.com", "GET"))
	missing, _ := serve(p, preflight("/nonexistent/doc.pdf", "https://app.example.com", "GET"))

	if existing.Code != missing.Code {
		t.Errorf("status %d for an existing bucket, %d for a missing one", existing.Code, missing.Code)
	}
	if existing.Body.String() != missing.Body.String() {
		t.Errorf("body %q for an existing bucket, %q for a missing one",
			existing.Body.String(), missing.Body.String())
	}
}

// TestMiddleware_PreflightWildcardOriginEchoesRequest verifies a rule
// admitting any origin still answers with the caller's origin rather than a
// bare wildcard, so the response stays correct under the Vary header a cache
// keys on.
func TestMiddleware_PreflightWildcardOriginEchoesRequest(t *testing.T) {
	t.Parallel()
	p := newTestPolicy(t)

	rec, _ := serve(p, preflight("/public/logo.png", "https://anything.test", "GET"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://anything.test" {
		t.Errorf("Allow-Origin = %q, want the request origin", got)
	}
}

// TestMiddleware_NilRulesRefuses verifies a policy that never had rules
// stored refuses rather than panicking, which is the state an instance is in
// when its reload failed.
func TestMiddleware_NilRulesRefuses(t *testing.T) {
	t.Parallel()
	p := New(bucketFromPath, writeTestError)

	rec, reached := serve(p, preflight("/photos/doc.pdf", "https://app.example.com", "GET"))

	if reached {
		t.Error("preflight reached the wrapped handler with no rules stored")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// -------------------------------------------------------------------------
// PASS-THROUGH AND DECORATION
// -------------------------------------------------------------------------

// TestMiddleware_NoOriginPassesThrough verifies a request from a server-side
// client is untouched, which is every request on a fleet serving no browsers.
func TestMiddleware_NoOriginPassesThrough(t *testing.T) {
	t.Parallel()
	p := newTestPolicy(t)

	rec, reached := serve(p, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/photos/doc.pdf", nil))

	if !reached {
		t.Error("request without an Origin did not reach the handler")
	}
	if got := rec.Header().Get("Vary"); got != "" {
		t.Errorf("Vary = %q, want it absent for a request that is not cross-origin", got)
	}
}

// TestMiddleware_OptionsWithoutRequestMethodIsNotPreflight verifies a bare
// OPTIONS is passed to the handler rather than answered, since it is not a
// preflight and the handler owns the response for it.
func TestMiddleware_OptionsWithoutRequestMethodIsNotPreflight(t *testing.T) {
	t.Parallel()
	p := newTestPolicy(t)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/photos/doc.pdf", nil)
	r.Header.Set("Origin", "https://app.example.com")

	_, reached := serve(p, r)

	if !reached {
		t.Error("bare OPTIONS was answered by the middleware, want it passed through")
	}
}

// TestMiddleware_DecoratesActualRequest verifies the allow and expose headers
// are set before the handler runs, which is what lets a browser upload read
// back the ETag of the object it just wrote.
func TestMiddleware_DecoratesActualRequest(t *testing.T) {
	t.Parallel()
	p := newTestPolicy(t)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/photos/doc.pdf", nil)
	r.Header.Set("Origin", "https://app.example.com")

	rec, reached := serve(p, r)

	if !reached {
		t.Fatal("decorated request did not reach the handler")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Allow-Origin = %q, want the request origin", got)
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "ETag" {
		t.Errorf("Expose-Headers = %q, want %q", got, "ETag")
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want %q", got, "Origin")
	}
}

// TestMiddleware_UnmatchedActualRequestIsNotRefused verifies a cross-origin
// request no rule admits still reaches the handler, undecorated. The Origin
// header is not proof of a browser, and refusing here would break a signed
// request from a client that merely sets it.
func TestMiddleware_UnmatchedActualRequestIsNotRefused(t *testing.T) {
	t.Parallel()
	p := newTestPolicy(t)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/photos/doc.pdf", nil)
	r.Header.Set("Origin", "https://evil.test")

	rec, reached := serve(p, r)

	if !reached {
		t.Error("unmatched cross-origin request was refused, want it passed through")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want it absent for an unmatched origin", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want %q so a cache cannot serve this to another origin", got, "Origin")
	}
}

// -------------------------------------------------------------------------
// RELOAD
// -------------------------------------------------------------------------

// TestPolicy_SetRulesReplacesTheSet verifies a swapped rule set takes effect
// on the next request, which is what makes adding an origin a reload rather
// than a restart.
func TestPolicy_SetRulesReplacesTheSet(t *testing.T) {
	t.Parallel()
	p := newTestPolicy(t)

	if rec, _ := serve(p, preflight("/photos/doc.pdf", "https://new.example.com", "GET")); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d before the swap, want %d", rec.Code, http.StatusForbidden)
	}

	reg, err := NewRegistry([]config.BucketConfig{{Name: "photos", CORS: []config.CORSRule{{
		AllowedOrigins: []string{"https://new.example.com"},
		AllowedMethods: []string{"GET"},
	}}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	p.SetRules(reg)

	rec, _ := serve(p, preflight("/photos/doc.pdf", "https://new.example.com", "GET"))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d after the swap, want %d", rec.Code, http.StatusOK)
	}
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// TestNew_PanicsOnMissingDependency verifies the wiring boundary refuses a
// nil collaborator at construction rather than at the first cross-origin
// request, which could be days later.
func TestNew_PanicsOnMissingDependency(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		resolve  BucketResolver
		writeErr func(http.ResponseWriter, int, string, string)
	}{
		{"nil resolver", nil, writeTestError},
		{"nil error writer", bucketFromPath, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("New did not panic on a nil dependency")
				}
			}()
			New(tc.resolve, tc.writeErr)
		})
	}
}
