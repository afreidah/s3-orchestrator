// -------------------------------------------------------------------------------
// Admin API - Route Table Invariants
//
// Author: Alex Freidah
//
// The route table is the single source the mux and the generated API
// description are both built from, so these tests pin the properties that
// make it trustworthy: every entry is complete, no pattern is registered
// twice, every entry is actually reachable, and the auth wrapper is applied
// by the loop rather than per entry.
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRoutes_EntriesAreIdentified asserts every entry carries the fields that
// identify it: a mux pattern under the admin prefix, a handler, and the
// summary the generated description uses as its operation summary.
func TestRoutes_EntriesAreIdentified(t *testing.T) {
	t.Parallel()
	for _, rt := range newTestHandler(t).routes() {
		name := rt.Method + " " + rt.Pattern
		if rt.Method == "" || rt.Pattern == "" {
			t.Errorf("entry %q is missing a method or pattern", name)
		}
		if rt.Handler == nil {
			t.Errorf("%s has no handler", name)
		}
		if rt.Summary == "" {
			t.Errorf("%s has no summary", name)
		}
		if !strings.HasPrefix(rt.Pattern, "/admin/api/") {
			t.Errorf("%s is not under /admin/api/", name)
		}
	}
}

// TestRoutes_EntriesDeclareAResponse asserts every entry says what it answers
// with. A route with no declared response would land in the generated
// description as an untyped endpoint, which is the drift this table exists to
// prevent.
func TestRoutes_EntriesDeclareAResponse(t *testing.T) {
	t.Parallel()
	for _, rt := range newTestHandler(t).routes() {
		name := rt.Method + " " + rt.Pattern
		// A nil Response is only legitimate when the route says it does not
		// answer in JSON, which today is the binary trace download.
		if rt.Response == nil && rt.ResponseType == "" {
			t.Errorf("%s declares neither a response type nor a non-JSON media type", name)
		}
		if rt.Response != nil && rt.ResponseType != "" {
			t.Errorf("%s declares both a JSON response and the media type %q", name, rt.ResponseType)
		}
	}
}

// TestRoutes_NoDuplicatePatterns guards against registering the same
// method+pattern twice, which net/http panics on at mount time.
func TestRoutes_NoDuplicatePatterns(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, rt := range newTestHandler(t).routes() {
		key := rt.Method + " " + rt.Pattern
		if seen[key] {
			t.Errorf("duplicate route entry %q", key)
		}
		seen[key] = true
	}
}

// TestRegister_MountsEveryEntry asserts each table entry resolves to its own
// registered pattern. If the table and the registration loop disagreed, the
// generated description would advertise an endpoint the server does not serve.
// Routing is resolved through mux.Handler rather than ServeHTTP so the check
// never enters a handler body, which would need every dependency wired.
func TestRegister_MountsEveryEntry(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	for _, rt := range h.routes() {
		// Substitute a concrete value for each wildcard so the request
		// matches the registered pattern.
		path := strings.NewReplacer("{name}", "b1", "{key...}", "some/key").Replace(rt.Pattern)
		req := httptest.NewRequestWithContext(t.Context(), rt.Method, path, nil)

		_, pattern := mux.Handler(req)
		if want := rt.Method + " " + rt.Pattern; pattern != want {
			t.Errorf("%s %s resolved to pattern %q, want %q", rt.Method, path, pattern, want)
		}
	}
}

// TestRegister_AppliesAuthToEveryEntry asserts the registration loop wraps
// every route with requireToken. Applying auth in the loop rather than per
// entry is what stops an endpoint shipping unauthenticated, so an unauthorized
// request must be rejected on every path in the table.
func TestRegister_AppliesAuthToEveryEntry(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	for _, rt := range h.routes() {
		path := strings.NewReplacer("{name}", "b1", "{key...}", "some/key").Replace(rt.Pattern)
		req := httptest.NewRequestWithContext(t.Context(), rt.Method, path, nil)
		// Deliberately no X-Admin-Token header.
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d without a token, want 401", rt.Method, path, w.Code)
		}
	}
}

// TestRoutes_StreamingEntriesDeclareTheEventType pins the dual-mode endpoints.
// They answer with their JSON response by default and with an
// NDJSON event stream when the caller asks for one, so both shapes have to be
// declared or the generated description would describe only half of each.
func TestRoutes_StreamingEntriesDeclareTheEventType(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		"POST /admin/api/rebalance":           true,
		"POST /admin/api/replicate":           true,
		"POST /admin/api/over-replication":    true,
		"POST /admin/api/scrub":               true,
		"POST /admin/api/backfill-checksums":  true,
		"POST /admin/api/reconcile":           true,
		"POST /admin/api/compress-existing":   true,
		"POST /admin/api/decompress-existing": true,
		"DELETE /admin/api/backends/{name}":   true,
	}
	for _, rt := range newTestHandler(t).routes() {
		key := rt.Method + " " + rt.Pattern
		switch {
		case want[key] && rt.Stream == nil:
			t.Errorf("%s streams NDJSON but declares no stream type", key)
		case !want[key] && rt.Stream != nil:
			t.Errorf("%s declares a stream type but does not stream", key)
		}
	}
}
