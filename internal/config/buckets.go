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

// The layers a bucket's stored_form may name. They are the names the ledger's
// stored-form flags render under, which take them from here.
const (
	LayerCompressed = "compressed"
	LayerEncrypted  = "encrypted"
	LayerHashed     = "hashed"
)

// bucketLayers is the set a stored_form entry may name.
var bucketLayers = map[string]bool{LayerCompressed: true, LayerEncrypted: true, LayerHashed: true}

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
	AllowedOrigins []string `yaml:"allowed_origins" json:"allowed_origins"`
	AllowedMethods []string `yaml:"allowed_methods" json:"allowed_methods"`
	AllowedHeaders []string `yaml:"allowed_headers" json:"allowed_headers,omitempty"`
	ExposeHeaders  []string `yaml:"expose_headers" json:"expose_headers,omitempty"`
	MaxAge         int      `yaml:"max_age" json:"max_age,omitempty"`
}

// BucketSettings is what a virtual bucket carries besides its name and
// credentials, in the one shape the config file, the store, the merged view
// and the admin API's requests all embed.
//
// MaxMultipartUploads of zero is unlimited, and a request may leave it out.
// CORS is empty by default, which refuses every cross-origin preflight.
// StoredForm names the layers a new object is stored with: nil takes the
// deployment's (compressed, encrypted and hashed as compression, encryption
// and integrity are enabled), and any list, even an empty one, replaces them
// all. ReplicationFactor is how many copies each object is kept at, zero for
// replication.factor.
type BucketSettings struct {
	MaxMultipartUploads int        `yaml:"max_multipart_uploads" json:"max_multipart_uploads,omitzero"`
	CORS                []CORSRule `yaml:"cors" json:"cors,omitempty"`
	StoredForm          *[]string  `yaml:"stored_form,omitempty" json:"stored_form,omitempty"`
	ReplicationFactor   int        `yaml:"replication_factor,omitempty" json:"replication_factor,omitzero"`
}

// BucketLimits is what the deployment allows a bucket's settings: the
// encrypted layer only while it encrypts, and a replication factor no larger
// than its fleet.
type BucketLimits struct {
	Encrypts bool
	Backends int
}

// BucketLimits returns the limits c sets a bucket's settings.
func (c *Config) BucketLimits() BucketLimits {
	return BucketLimits{Encrypts: c.Encryption.Enabled, Backends: len(c.Backends)}
}

// BucketConfig defines a virtual bucket with one or more credential sets.
// Multiple services can share a bucket by each having their own credentials.
type BucketConfig struct {
	Name           string             `yaml:"name"`
	Credentials    []CredentialConfig `yaml:"credentials"`
	BucketSettings `yaml:",inline"`
}

// validateBuckets enforces unique names and at least one credential per
// bucket. Declaring none is allowed, since buckets can live in the store.
func validateBuckets(buckets []BucketConfig, limits BucketLimits) []error {
	var errs []error

	seen := newSeenCredentials()
	for i := range buckets {
		errs = append(errs, validateBucket(i, &buckets[i], seen, limits)...)
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
// config path, since the caller has no file to point at. limits is what the
// running deployment allows the bucket's settings.
func ValidateBucket(bkt *BucketConfig, limits BucketLimits) []error {
	return validateBucketFields("bucket", bkt, limits)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// validateBucket checks a single bucket entry. seen is shared across the full
// list so duplicates are detected across bucket boundaries.
func validateBucket(idx int, bkt *BucketConfig, seen *seenCredentials, limits BucketLimits) []error {
	prefix := fmt.Sprintf("buckets[%d]", idx)
	errs := validateBucketFields(prefix, bkt, limits)

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
// it is written to, and the replication factor by the fleet a copy can go to.
func validateBucketFields(prefix string, bkt *BucketConfig, limits BucketLimits) []error {
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
	if f := bkt.ReplicationFactor; f < 0 || f > limits.Backends {
		errs = append(errs, prefixedDetail(prefix, ErrBucketFactorRange,
			fmt.Sprintf("factor=%d backends=%d", f, limits.Backends)))
	}
	for j := range bkt.CORS {
		errs = append(errs, validateCORSRule(prefix, j, &bkt.CORS[j])...)
	}
	if bkt.StoredForm != nil {
		errs = append(errs, validateStoredForm(prefix, *bkt.StoredForm, limits.Encrypts)...)
	}
	return errs
}

// validateStoredForm checks each layer a bucket names. Encrypted needs the key
// source an enabled encryption section loads, or the bucket's writes could
// only be refused.
func validateStoredForm(prefix string, layers []string, encrypts bool) []error {
	var errs []error
	for _, layer := range layers {
		switch {
		case !bucketLayers[layer]:
			errs = append(errs, prefixedDetail(prefix, ErrBucketLayerUnknown, fmt.Sprintf("%q", layer)))
		case layer == LayerEncrypted && !encrypts:
			errs = append(errs, prefixed(prefix, ErrBucketLayerNoKey))
		}
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
