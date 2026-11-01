// -------------------------------------------------------------------------------
// Bucket Configuration
//
// Author: Alex Freidah
//
// Defines virtual-bucket configuration: the public bucket name clients see,
// the credential bundles that authorize access, the browser origins allowed
// to reach the bucket cross-origin, and the bucket-prefix convention used to
// namespace objects in the underlying physical backends. Validators enforce
// that every bucket has at least one credential and that access keys are
// unique across buckets so SigV4 resolution is unambiguous.
// -------------------------------------------------------------------------------

package config

import (
	"fmt"
	"math"
	"strings"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// corsMethods is the set an allowed_methods entry may name: the methods the
// S3 transport implements. A rule naming anything else is refused at load
// rather than silently never matching a request.
var corsMethods = map[string]bool{
	"GET":    true,
	"HEAD":   true,
	"PUT":    true,
	"POST":   true,
	"DELETE": true,
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// CredentialConfig holds a single SigV4 keypair for accessing a virtual
// bucket: access_key_id and secret_access_key, both required.
type CredentialConfig struct {
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
}

// CORSRule declares which browser origins may reach a bucket cross-origin,
// mirroring the S3 CORSRule shape.
//
// MaxAge is the seconds a browser may cache a preflight result; zero leaves
// the header off. Without ETag in ExposeHeaders, a browser upload cannot read
// the ETag of the object it just wrote.
type CORSRule struct {
	AllowedOrigins []string `yaml:"allowed_origins"`
	AllowedMethods []string `yaml:"allowed_methods"`
	AllowedHeaders []string `yaml:"allowed_headers"`
	ExposeHeaders  []string `yaml:"expose_headers"`
	MaxAge         int      `yaml:"max_age"`
}

// BucketConfig defines a virtual bucket with one or more credential sets.
// Multiple services can share a bucket by each having their own credentials.
//
// CORS is empty by default, which refuses every cross-origin preflight. A
// bucket only reachable from server-side clients never needs it.
type BucketConfig struct {
	Name                string             `yaml:"name"`
	Credentials         []CredentialConfig `yaml:"credentials"`
	MaxMultipartUploads int                `yaml:"max_multipart_uploads"` // Max active multipart uploads per bucket (0 = unlimited)
	CORS                []CORSRule         `yaml:"cors"`
}

// validateBuckets enforces unique names and at least one credential per
// bucket. Declaring none is allowed, since buckets can live in the store.
func validateBuckets(buckets []BucketConfig) []error {
	var errs []error

	seen := newSeenCredentials()
	for i := range buckets {
		errs = append(errs, validateBucket(i, &buckets[i], seen)...)
	}
	return errs
}

// seenCredentials tracks the identifiers that have to be unique across every
// bucket. Uniqueness is a security property: each identifier resolves a
// request to the bucket it may read and write, so a duplicate would let
// whichever bucket the registry stored last silently own it.
type seenCredentials struct {
	names      map[string]bool
	accessKeys map[string]bool
}

// newSeenCredentials builds an empty tracker.
func newSeenCredentials() *seenCredentials {
	return &seenCredentials{
		names:      make(map[string]bool),
		accessKeys: make(map[string]bool),
	}
}

// ValidateBucket checks a bucket outside a config file, for the provisioning
// API, which accepts the same fields over the wire and has to refuse what config
// would before it reaches the store. Credentials are not checked, since API
// buckets carry none. Messages are prefixed as the bucket's own rather than by
// config path, since the caller has no file to point at.
func ValidateBucket(bkt *BucketConfig) []error {
	return validateBucketFields("bucket", bkt)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// validateBucket checks a single bucket entry. seen is shared across the full
// list so duplicates are detected across bucket boundaries.
func validateBucket(idx int, bkt *BucketConfig, seen *seenCredentials) []error {
	prefix := fmt.Sprintf("buckets[%d]", idx)
	errs := validateBucketFields(prefix, bkt)

	if seen.names[bkt.Name] {
		errs = append(errs, prefixedDetail(prefix, ErrDuplicateBucketName, fmt.Sprintf("%q", bkt.Name)))
	}
	seen.names[bkt.Name] = true

	if len(bkt.Credentials) == 0 {
		errs = append(errs, prefixed(prefix, ErrNoCredential))
	}
	for j := range bkt.Credentials {
		errs = append(errs, validateCredential(prefix, j, &bkt.Credentials[j], seen)...)
	}
	return errs
}

// validateBucketFields checks what a bucket carries apart from its credentials
// and its uniqueness among the others, which is all a bucket created over the
// provisioning API has. The upload limit is bounded above by the store column
// it is written to.
func validateBucketFields(prefix string, bkt *BucketConfig) []error {
	var errs []error
	if bkt.Name == "" {
		errs = append(errs, prefixed(prefix, ErrBucketNameRequired))
	}
	if strings.Contains(bkt.Name, "/") {
		errs = append(errs, prefixed(prefix, ErrBucketNameHasSlash))
	}
	if bkt.MaxMultipartUploads < 0 || bkt.MaxMultipartUploads > math.MaxInt32 {
		errs = append(errs, prefixed(prefix, ErrMaxUploadsRange))
	}
	for j := range bkt.CORS {
		errs = append(errs, validateCORSRule(prefix, j, &bkt.CORS[j])...)
	}
	return errs
}

// validateCORSRule checks a single CORS rule within a bucket. A rule that
// cannot match anything is an error rather than a warning: it reads as
// granting access the operator never gets, and the browser failure it causes
// is reported client-side as an opaque CORS error with nothing in the server
// log to connect it to.
func validateCORSRule(bucketPrefix string, idx int, rule *CORSRule) []error {
	prefix := fmt.Sprintf("%s.cors[%d]", bucketPrefix, idx)
	var errs []error

	if len(rule.AllowedOrigins) == 0 {
		errs = append(errs, prefixed(prefix, ErrCORSNoOrigins))
	}
	for _, origin := range rule.AllowedOrigins {
		if origin == "" {
			errs = append(errs, prefixed(prefix, ErrCORSEmptyOrigin))
			continue
		}
		if strings.Count(origin, "*") > 1 {
			errs = append(errs, prefixedDetail(prefix, ErrCORSOriginWildcard, fmt.Sprintf("%q", origin)))
		}
	}

	if len(rule.AllowedMethods) == 0 {
		errs = append(errs, prefixed(prefix, ErrCORSNoMethods))
	}
	for _, method := range rule.AllowedMethods {
		if !corsMethods[strings.ToUpper(method)] {
			errs = append(errs, prefixedDetail(prefix, ErrCORSBadMethod, fmt.Sprintf("%q", method)))
		}
	}

	if rule.MaxAge < 0 {
		errs = append(errs, prefixed(prefix, ErrCORSNegativeMaxAge))
	}
	return errs
}

// validateCredential checks a single credential entry within a bucket.
func validateCredential(bucketPrefix string, idx int, cred *CredentialConfig, seen *seenCredentials) []error {
	prefix := fmt.Sprintf("%s.credentials[%d]", bucketPrefix, idx)
	var errs []error

	if cred.AccessKeyID == "" || cred.SecretAccessKey == "" {
		errs = append(errs, prefixed(prefix, ErrInvalidCredential))
	}
	if cred.AccessKeyID != "" {
		if seen.accessKeys[cred.AccessKeyID] {
			errs = append(errs, prefixedDetail(prefix, ErrDuplicateCredential, fmt.Sprintf("%q", cred.AccessKeyID)))
		}
		seen.accessKeys[cred.AccessKeyID] = true
	}
	return errs
}
