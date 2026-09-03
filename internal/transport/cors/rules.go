// -------------------------------------------------------------------------------
// CORS Rules - Compiled Per-Bucket Match Set
//
// Author: Alex Freidah
//
// Compiles the CORS rules an operator declared per virtual bucket into the
// form the request path matches against: origin patterns split around their
// wildcard, a method set, header patterns lower-cased for case-insensitive
// comparison, and the response header values pre-rendered. Matching is a
// read-only operation on the compiled set, so the registry is safe to share
// across requests and to replace wholesale on reload.
// -------------------------------------------------------------------------------

package cors

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Registry is the compiled rule set, keyed by virtual bucket name. A bucket
// with no entry has no CORS rules and refuses every preflight.
type Registry struct {
	byBucket map[string][]rule
}

// rule is one compiled CORSRule.
//
// allowMethods, exposeHeaders and maxAge hold the response header values
// rather than the parsed lists they came from, because every match writes
// them verbatim and nothing on the request path needs to read them back.
// maxAge is empty when the operator left it at zero, which leaves the header
// off and lets the browser apply its own default.
type rule struct {
	origins       []pattern
	methods       map[string]bool
	headers       []pattern
	allowMethods  string
	exposeHeaders string
	maxAge        string
}

// pattern matches an origin or a header name, either exactly or around the
// single '*' the value is allowed to carry. Splitting at compile time keeps
// the request path free of wildcard parsing.
type pattern struct {
	prefix string
	suffix string
	wild   bool
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewRegistry compiles the CORS rules declared on every bucket. Buckets
// without rules are left out of the map entirely, so the common case of a
// fleet that serves no browsers costs one failed lookup per cross-origin
// request and nothing at all otherwise.
//
// Returns an error for a pattern the matcher cannot read. Config validation
// rejects the same shapes with an operator-facing message; this is the
// backstop that keeps a gap there from compiling into a rule that silently
// matches more origins than the operator wrote.
func NewRegistry(buckets []config.BucketConfig) (*Registry, error) {
	reg := &Registry{byBucket: make(map[string][]rule)}
	for i := range buckets {
		bkt := &buckets[i]
		if len(bkt.CORS) == 0 {
			continue
		}
		compiled := make([]rule, 0, len(bkt.CORS))
		for j := range bkt.CORS {
			r, err := compileRule(&bkt.CORS[j])
			if err != nil {
				return nil, fmt.Errorf("bucket %q cors[%d]: %w", bkt.Name, j, err)
			}
			compiled = append(compiled, r)
		}
		reg.byBucket[bkt.Name] = compiled
	}
	return reg, nil
}

// -------------------------------------------------------------------------
// MATCHING
// -------------------------------------------------------------------------

// matchPreflight returns the first rule admitting the origin, the method the
// browser announced it intends to use, and every header it announced it
// intends to send. A rule that admits the origin and method but not one of
// the headers does not match, so a later rule still gets its turn.
//
// Header names are folded here rather than by the caller: case-insensitivity
// is a property of a header name, so the matcher owning it means no caller
// can forget and silently narrow the rule to whichever casing it passed.
func (reg *Registry) matchPreflight(bucket, origin, method string, headers []string) *rule {
	return reg.find(bucket, origin, method, func(r *rule) bool {
		for _, h := range headers {
			if !matchAny(r.headers, strings.ToLower(h)) {
				return false
			}
		}
		return true
	})
}

// matchActual returns the first rule admitting the origin and method of a
// request that is not a preflight. Request headers are not consulted: the
// browser already cleared them against the preflight, and a non-browser
// caller that happens to send an Origin is not restricted by CORS at all.
func (reg *Registry) matchActual(bucket, origin, method string) *rule {
	return reg.find(bucket, origin, method, nil)
}

// find walks the bucket's rules in declaration order and returns the first
// admitting the origin and method, subject to extra. Order is the operator's:
// a broad rule written first shadows a narrower one after it, the same way it
// does on S3.
func (reg *Registry) find(bucket, origin, method string, extra func(*rule) bool) *rule {
	rules := reg.byBucket[bucket]
	for i := range rules {
		r := &rules[i]
		if !matchAny(r.origins, origin) || !r.methods[method] {
			continue
		}
		if extra != nil && !extra(r) {
			continue
		}
		return r
	}
	return nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// compileRule turns one declared rule into its matchable form.
func compileRule(src *config.CORSRule) (rule, error) {
	origins, err := compilePatterns(src.AllowedOrigins, false)
	if err != nil {
		return rule{}, err
	}
	headers, err := compilePatterns(src.AllowedHeaders, true)
	if err != nil {
		return rule{}, err
	}

	methods := make(map[string]bool, len(src.AllowedMethods))
	upper := make([]string, 0, len(src.AllowedMethods))
	for _, m := range src.AllowedMethods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m == "" || methods[m] {
			continue
		}
		methods[m] = true
		upper = append(upper, m)
	}

	maxAge := ""
	if src.MaxAge > 0 {
		maxAge = strconv.Itoa(src.MaxAge)
	}

	return rule{
		origins:       origins,
		methods:       methods,
		headers:       headers,
		allowMethods:  strings.Join(upper, ", "),
		exposeHeaders: strings.Join(src.ExposeHeaders, ", "),
		maxAge:        maxAge,
	}, nil
}

// compilePatterns compiles a list of origin or header patterns. Header
// patterns are lower-cased so comparison against an incoming header name is
// case-insensitive, as HTTP requires; origins are compared as written because
// an origin is a URL and its host casing is the client's to choose.
func compilePatterns(values []string, lower bool) ([]pattern, error) {
	out := make([]pattern, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if lower {
			v = strings.ToLower(v)
		}
		p, err := compilePattern(v)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// compilePattern splits a value around its wildcard.
func compilePattern(v string) (pattern, error) {
	switch strings.Count(v, "*") {
	case 0:
		return pattern{prefix: v}, nil
	case 1:
		star := strings.IndexByte(v, '*')
		return pattern{prefix: v[:star], suffix: v[star+1:], wild: true}, nil
	default:
		return pattern{}, fmt.Errorf("%q carries more than one '*'", v)
	}
}

// matchAny reports whether any pattern admits the value.
func matchAny(patterns []pattern, value string) bool {
	for i := range patterns {
		if patterns[i].matches(value) {
			return true
		}
	}
	return false
}

// matches reports whether the pattern admits the value. The length guard is
// what stops a wildcard pattern from matching a value shorter than its own
// literal parts, where the prefix and suffix would otherwise overlap.
func (p pattern) matches(value string) bool {
	if !p.wild {
		return value == p.prefix
	}
	return len(value) >= len(p.prefix)+len(p.suffix) &&
		strings.HasPrefix(value, p.prefix) &&
		strings.HasSuffix(value, p.suffix)
}
