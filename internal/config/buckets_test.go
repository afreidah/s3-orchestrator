// -------------------------------------------------------------------------------
// Bucket Configuration Tests
//
// Author: Alex Freidah
//
// Covers CORS rule validation. Every case here is a rule that would compile
// but never match a request, which is worth refusing at load: the browser
// reports the resulting failure as an opaque CORS error with nothing on the
// server side connecting it back to the rule that was mistyped.
// -------------------------------------------------------------------------------

package config

import (
	"errors"
	"strings"
	"testing"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// bucketWithCORS builds a minimal valid bucket carrying one CORS rule, so a
// case only has to state the part of the rule it is about.
func bucketWithCORS(rule *CORSRule) []BucketConfig {
	return []BucketConfig{{
		Name:        "photos",
		Credentials: []CredentialConfig{{AccessKeyID: "ak", SecretAccessKey: "sk"}},
		CORS:        []CORSRule{*rule},
	}}
}

// hasError reports whether any error in the slice wraps target.
func hasError(errs []error, target error) bool {
	for _, err := range errs {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------------
// CORS VALIDATION
// -------------------------------------------------------------------------

// TestValidateBuckets_CORSRules pins each way a rule is refused, and the
// shapes that must keep loading.
func TestValidateBuckets_CORSRules(t *testing.T) {
	t.Parallel()
	valid := CORSRule{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"GET", "PUT"},
	}
	cases := []struct {
		name    string
		rule    CORSRule
		wantErr error
	}{
		{"valid rule", valid, nil},
		{
			"wildcard origin",
			CORSRule{AllowedOrigins: []string{"https://*.example.com"}, AllowedMethods: []string{"GET"}},
			nil,
		},
		{
			"bare wildcard origin",
			CORSRule{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}},
			nil,
		},
		{
			"lower-case method",
			CORSRule{AllowedOrigins: []string{"https://a.example.com"}, AllowedMethods: []string{"get"}},
			nil,
		},
		{
			"no origins",
			CORSRule{AllowedMethods: []string{"GET"}},
			ErrCORSNoOrigins,
		},
		{
			"empty origin entry",
			CORSRule{AllowedOrigins: []string{""}, AllowedMethods: []string{"GET"}},
			ErrCORSEmptyOrigin,
		},
		{
			"two wildcards in one origin",
			CORSRule{AllowedOrigins: []string{"https://*.*.example.com"}, AllowedMethods: []string{"GET"}},
			ErrCORSOriginWildcard,
		},
		{
			"no methods",
			CORSRule{AllowedOrigins: []string{"https://a.example.com"}},
			ErrCORSNoMethods,
		},
		{
			"unsupported method",
			CORSRule{AllowedOrigins: []string{"https://a.example.com"}, AllowedMethods: []string{"PATCH"}},
			ErrCORSBadMethod,
		},
		{
			"negative max age",
			CORSRule{AllowedOrigins: []string{"https://a.example.com"}, AllowedMethods: []string{"GET"}, MaxAge: -1},
			ErrCORSNegativeMaxAge,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := validateBuckets(bucketWithCORS(&tc.rule))
			if tc.wantErr == nil {
				if len(errs) != 0 {
					t.Errorf("validateBuckets() = %v, want no errors", errs)
				}
				return
			}
			if !hasError(errs, tc.wantErr) {
				t.Errorf("validateBuckets() = %v, want an error wrapping %v", errs, tc.wantErr)
			}
		})
	}
}

// TestValidateBuckets_CORSAbsentIsValid verifies a bucket that declares no
// rules is not treated as a bucket with an empty rule set, so the common
// server-side-only deployment does not have to opt out of a feature it never
// asked for.
func TestValidateBuckets_CORSAbsentIsValid(t *testing.T) {
	t.Parallel()
	errs := validateBuckets([]BucketConfig{{
		Name:        "photos",
		Credentials: []CredentialConfig{{AccessKeyID: "ak", SecretAccessKey: "sk"}},
	}})
	if len(errs) != 0 {
		t.Errorf("validateBuckets() = %v, want no errors", errs)
	}
}

// TestValidateBuckets_CORSErrorNamesTheRule verifies the positional prefix
// reaches the message, so an operator with several rules per bucket is told
// which one is wrong rather than that one of them is.
func TestValidateBuckets_CORSErrorNamesTheRule(t *testing.T) {
	t.Parallel()
	buckets := []BucketConfig{{
		Name:        "photos",
		Credentials: []CredentialConfig{{AccessKeyID: "ak", SecretAccessKey: "sk"}},
		CORS: []CORSRule{
			{AllowedOrigins: []string{"https://a.example.com"}, AllowedMethods: []string{"GET"}},
			{AllowedOrigins: []string{"https://b.example.com"}},
		},
	}}

	errs := validateBuckets(buckets)
	if !hasError(errs, ErrCORSNoMethods) {
		t.Fatalf("validateBuckets() = %v, want an error wrapping %v", errs, ErrCORSNoMethods)
	}
	found := false
	for _, err := range errs {
		if errors.Is(err, ErrCORSNoMethods) && strings.Contains(err.Error(), "buckets[0].cors[1]") {
			found = true
		}
	}
	if !found {
		t.Errorf("errors %v do not identify buckets[0].cors[1]", errs)
	}
}
