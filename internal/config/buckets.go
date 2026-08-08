// -------------------------------------------------------------------------------
// Bucket Configuration
//
// Author: Alex Freidah
//
// Defines virtual-bucket configuration: the public bucket name clients see,
// the credential bundles that authorize access, and the bucket-prefix
// convention used to namespace objects in the underlying physical
// backends. Validators enforce that every bucket has at least one
// credential and that access keys are unique across buckets so SigV4
// resolution is unambiguous.
// -------------------------------------------------------------------------------

package config

import (
	"fmt"
	"strings"
)

// CredentialConfig holds a single set of client credentials for accessing a
// virtual bucket. Supports SigV4 (access_key_id + secret_access_key) or legacy
// token auth.
type CredentialConfig struct {
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	Token           string `yaml:"token"`
}

// BucketConfig defines a virtual bucket with one or more credential sets.
// Multiple services can share a bucket by each having their own credentials.
type BucketConfig struct {
	Name                string             `yaml:"name"`
	Credentials         []CredentialConfig `yaml:"credentials"`
	MaxMultipartUploads int                `yaml:"max_multipart_uploads"` // Max active multipart uploads per bucket (0 = unlimited)
}

// validateBuckets enforces that at least one virtual bucket is
// configured, every bucket has a unique name, and every bucket carries
// at least one credential pair so SigV4 resolution has something to
// match. Errors aggregate so operators see all problems in one pass.
func validateBuckets(buckets []BucketConfig) []error {
	var errs []error

	if len(buckets) == 0 {
		errs = append(errs, ErrNoBuckets)
	}

	seen := newSeenCredentials()
	for i := range buckets {
		errs = append(errs, validateBucket(i, &buckets[i], seen)...)
	}
	return errs
}

// seenCredentials tracks the identifiers that have to be unique across every
// bucket, so a duplicate is caught wherever in the list it appears.
//
// Uniqueness is a security property here, not tidiness: each of these
// identifiers resolves an inbound request to the bucket whose namespace it may
// read and write, so two buckets claiming one identifier means whichever the
// registry stored last silently owns it.
type seenCredentials struct {
	names      map[string]bool
	accessKeys map[string]bool
	tokens     map[string]bool
}

// newSeenCredentials builds an empty tracker.
func newSeenCredentials() *seenCredentials {
	return &seenCredentials{
		names:      make(map[string]bool),
		accessKeys: make(map[string]bool),
		tokens:     make(map[string]bool),
	}
}

// validateBucket checks a single bucket entry. seen is shared across the full
// list so duplicates are detected across bucket boundaries.
func validateBucket(idx int, bkt *BucketConfig, seen *seenCredentials) []error {
	prefix := fmt.Sprintf("buckets[%d]", idx)
	var errs []error

	if bkt.Name == "" {
		errs = append(errs, prefixed(prefix, ErrBucketNameRequired))
	}
	if strings.Contains(bkt.Name, "/") {
		errs = append(errs, prefixed(prefix, ErrBucketNameHasSlash))
	}
	if seen.names[bkt.Name] {
		errs = append(errs, prefixedDetail(prefix, ErrDuplicateBucketName, fmt.Sprintf("%q", bkt.Name)))
	}
	seen.names[bkt.Name] = true

	if bkt.MaxMultipartUploads < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativeMaxUploads))
	}
	if len(bkt.Credentials) == 0 {
		errs = append(errs, prefixed(prefix, ErrNoCredential))
	}

	for j := range bkt.Credentials {
		errs = append(errs, validateCredential(prefix, j, &bkt.Credentials[j], seen)...)
	}
	return errs
}

// validateCredential checks a single credential entry within a bucket.
func validateCredential(bucketPrefix string, idx int, cred *CredentialConfig, seen *seenCredentials) []error {
	prefix := fmt.Sprintf("%s.credentials[%d]", bucketPrefix, idx)
	var errs []error

	hasSigV4 := cred.AccessKeyID != "" && cred.SecretAccessKey != ""
	hasToken := cred.Token != ""
	if !hasSigV4 && !hasToken {
		errs = append(errs, prefixed(prefix, ErrInvalidCredential))
	}
	if cred.AccessKeyID != "" {
		if seen.accessKeys[cred.AccessKeyID] {
			errs = append(errs, prefixedDetail(prefix, ErrDuplicateCredential, fmt.Sprintf("%q", cred.AccessKeyID)))
		}
		seen.accessKeys[cred.AccessKeyID] = true
	}
	// The token itself is the secret, so it is never echoed in the error.
	if hasToken {
		if seen.tokens[cred.Token] {
			errs = append(errs, prefixed(prefix, ErrDuplicateToken))
		}
		seen.tokens[cred.Token] = true
	}
	return errs
}
