// -------------------------------------------------------------------------------
// CORS Rule Tests
//
// Author: Alex Freidah
//
// Covers pattern compilation and the three matching paths: origin, method,
// and the request headers a preflight announces. The wildcard cases carry the
// weight - a pattern that matches more origins than the operator wrote is a
// cross-origin grant nobody asked for, and it fails open rather than loudly.
// -------------------------------------------------------------------------------

package cors

import (
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// registryWith compiles a single-bucket registry, failing the test on a
// compile error so cases stay free of error plumbing.
func registryWith(t *testing.T, rules ...config.CORSRule) *Registry {
	t.Helper()
	reg, err := NewRegistry([]config.BucketConfig{{Name: "photos", CORS: rules}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// -------------------------------------------------------------------------
// PATTERN MATCHING
// -------------------------------------------------------------------------

// TestPattern_Matches pins wildcard semantics, including the overlap case
// that a naive prefix-and-suffix test gets wrong.
func TestPattern_Matches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pattern string
		value   string
		want    bool
	}{
		{"exact match", "https://app.example.com", "https://app.example.com", true},
		{"exact mismatch", "https://app.example.com", "https://other.example.com", false},
		{"exact is not a prefix match", "https://app.example.com", "https://app.example.com.evil.test", false},
		{"bare wildcard matches anything", "*", "https://anything.test", true},
		{"bare wildcard matches empty", "*", "", true},
		{"leading wildcard", "*.example.com", "https://app.example.com", true},
		{"middle wildcard", "https://*.example.com", "https://app.example.com", true},
		{"middle wildcard rejects other host", "https://*.example.com", "https://app.evil.test", false},
		{"middle wildcard spans subdomains", "https://*.example.com", "https://a.b.example.com", true},
		{"trailing wildcard", "https://app.*", "https://app.example.com", true},
		{"wildcard may match nothing", "https://*.example.com", "https://.example.com", true},
		{"value shorter than the literal parts", "https://*.example.com", "https://", false},
		{"prefix and suffix may not overlap", "abc*bcd", "abcd", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := compilePattern(tc.pattern)
			if err != nil {
				t.Fatalf("compilePattern(%q): %v", tc.pattern, err)
			}
			if got := p.matches(tc.value); got != tc.want {
				t.Errorf("pattern %q matches(%q) = %t, want %t", tc.pattern, tc.value, got, tc.want)
			}
		})
	}
}

// TestCompilePattern_RejectsSecondWildcard verifies the backstop behind
// config validation: a pattern the matcher cannot read fails the compile
// rather than resolving to whichever reading the split happens to produce.
func TestCompilePattern_RejectsSecondWildcard(t *testing.T) {
	t.Parallel()
	if _, err := compilePattern("https://*.*.example.com"); err == nil {
		t.Error("compilePattern accepted two wildcards, want an error")
	}
}

// -------------------------------------------------------------------------
// REGISTRY
// -------------------------------------------------------------------------

// TestNewRegistry_SkipsBucketsWithoutRules verifies a bucket declaring no
// rules gets no entry, so the lookup for the common server-side-only bucket
// misses immediately.
func TestNewRegistry_SkipsBucketsWithoutRules(t *testing.T) {
	t.Parallel()
	reg, err := NewRegistry([]config.BucketConfig{
		{Name: "plain"},
		{Name: "photos", CORS: []config.CORSRule{{
			AllowedOrigins: []string{"https://app.example.com"},
			AllowedMethods: []string{"GET"},
		}}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, ok := reg.byBucket["plain"]; ok {
		t.Error("bucket without rules got a registry entry")
	}
	if _, ok := reg.byBucket["photos"]; !ok {
		t.Error("bucket with rules is missing from the registry")
	}
}

// TestNewRegistry_NamesTheOffendingRule verifies a compile failure identifies
// the bucket and rule index, since the reload path surfaces this error to an
// operator with no other context.
func TestNewRegistry_NamesTheOffendingRule(t *testing.T) {
	t.Parallel()
	_, err := NewRegistry([]config.BucketConfig{{Name: "photos", CORS: []config.CORSRule{
		{AllowedOrigins: []string{"https://ok.example.com"}, AllowedMethods: []string{"GET"}},
		{AllowedOrigins: []string{"https://*.*.example.com"}, AllowedMethods: []string{"GET"}},
	}}})
	if err == nil {
		t.Fatal("NewRegistry accepted an unreadable pattern, want an error")
	}
	if got := err.Error(); !strings.Contains(got, `bucket "photos" cors[1]`) {
		t.Errorf("error %q does not identify the rule", got)
	}
}

// -------------------------------------------------------------------------
// MATCHING
// -------------------------------------------------------------------------

// TestRegistry_MatchActual covers origin and method selection for a request
// that is not a preflight.
func TestRegistry_MatchActual(t *testing.T) {
	t.Parallel()
	reg := registryWith(t, config.CORSRule{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"GET", "PUT"},
	})
	cases := []struct {
		name   string
		bucket string
		origin string
		method string
		want   bool
	}{
		{"allowed origin and method", "photos", "https://app.example.com", "GET", true},
		{"second allowed method", "photos", "https://app.example.com", "PUT", true},
		{"method not allowed", "photos", "https://app.example.com", "DELETE", false},
		{"origin not allowed", "photos", "https://evil.test", "GET", false},
		{"bucket has no rules", "documents", "https://app.example.com", "GET", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := reg.matchActual(tc.bucket, tc.origin, tc.method)
			if (got != nil) != tc.want {
				t.Errorf("matchActual(%q, %q, %q) matched = %t, want %t",
					tc.bucket, tc.origin, tc.method, got != nil, tc.want)
			}
		})
	}
}

// TestRegistry_MatchPreflightHeaders verifies the announced request headers
// participate in the match, case-insensitively and through a wildcard.
func TestRegistry_MatchPreflightHeaders(t *testing.T) {
	t.Parallel()
	reg := registryWith(t, config.CORSRule{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"PUT"},
		AllowedHeaders: []string{"Content-Type", "x-amz-*"},
	})
	cases := []struct {
		name    string
		headers []string
		want    bool
	}{
		{"no headers announced", nil, true},
		{"exact header", []string{"content-type"}, true},
		{"header case is ignored", []string{"Content-Type"}, true},
		{"wildcard family", []string{"x-amz-date"}, true},
		{"one disallowed header fails the rule", []string{"content-type", "x-custom"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := reg.matchPreflight("photos", "https://app.example.com", "PUT", tc.headers)
			if (got != nil) != tc.want {
				t.Errorf("matchPreflight(headers=%v) matched = %t, want %t", tc.headers, got != nil, tc.want)
			}
		})
	}
}

// TestRegistry_MatchPreflightFallsThrough verifies a rule that admits the
// origin and method but not the headers does not stop the search, so a
// narrower rule written first cannot mask a broader one after it.
func TestRegistry_MatchPreflightFallsThrough(t *testing.T) {
	t.Parallel()
	reg := registryWith(t,
		config.CORSRule{
			AllowedOrigins: []string{"https://app.example.com"},
			AllowedMethods: []string{"PUT"},
			AllowedHeaders: []string{"content-type"},
		},
		config.CORSRule{
			AllowedOrigins: []string{"https://app.example.com"},
			AllowedMethods: []string{"PUT"},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag"},
		},
	)

	matched := reg.matchPreflight("photos", "https://app.example.com", "PUT", []string{"x-custom"})
	if matched == nil {
		t.Fatal("no rule matched, want the wildcard-header rule")
	}
	if matched.exposeHeaders != "ETag" {
		t.Errorf("matched the first rule; expose = %q, want the second rule's %q", matched.exposeHeaders, "ETag")
	}
}

// -------------------------------------------------------------------------
// COMPILED VALUES
// -------------------------------------------------------------------------

// TestCompileRule_RendersResponseValues verifies the response header strings
// are built once at compile time and normalised, since every match writes
// them verbatim.
func TestCompileRule_RendersResponseValues(t *testing.T) {
	t.Parallel()
	r, err := compileRule(&config.CORSRule{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"get", "PUT", "get"},
		ExposeHeaders:  []string{"ETag", "Content-Length"},
		MaxAge:         3600,
	})
	if err != nil {
		t.Fatalf("compileRule: %v", err)
	}
	if r.allowMethods != "GET, PUT" {
		t.Errorf("allowMethods = %q, want %q", r.allowMethods, "GET, PUT")
	}
	if r.exposeHeaders != "ETag, Content-Length" {
		t.Errorf("exposeHeaders = %q, want %q", r.exposeHeaders, "ETag, Content-Length")
	}
	if r.maxAge != "3600" {
		t.Errorf("maxAge = %q, want %q", r.maxAge, "3600")
	}
}

// TestCompileRule_ZeroMaxAgeOmitsHeader verifies an unset max_age renders
// empty, which leaves the header off so the browser applies its own default
// rather than being told not to cache at all.
func TestCompileRule_ZeroMaxAgeOmitsHeader(t *testing.T) {
	t.Parallel()
	r, err := compileRule(&config.CORSRule{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET"},
	})
	if err != nil {
		t.Fatalf("compileRule: %v", err)
	}
	if r.maxAge != "" {
		t.Errorf("maxAge = %q, want empty", r.maxAge)
	}
}
