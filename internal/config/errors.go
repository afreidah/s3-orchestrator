// -------------------------------------------------------------------------------
// Config Validation Sentinel Errors
//
// Author: Alex Freidah
//
// Every config validator wraps a sentinel defined below rather than returning
// a joined string. A string forces tests to match on substrings, which makes
// the message a de-facto API: a harmless reword silently breaks assertions.
//
// Most field checks take one of four shapes, a missing value, a value that
// must be positive, one that must not be negative, and one outside a range,
// and wrap a shared sentinel with the field's path through the helpers below.
// The rest wrap a sentinel of their own. Tests check with errors.Is; only the
// sentinel value is the contract.
// -------------------------------------------------------------------------------

package config

import (
	"errors"
	"fmt"
)

// -------------------------------------------------------------------------
// FIELD CHECKS
// -------------------------------------------------------------------------

// number is every field type the value checks compare: counts, sizes, ratios
// and durations.
type number interface {
	~int | ~int32 | ~int64 | ~float64
}

// required reports path as missing unless set holds.
func required(path string, set bool) error {
	if set {
		return nil
	}
	return fmt.Errorf("%s %w", path, ErrRequired)
}

// positive reports path unless v is above zero, for a count, size or interval
// that zero or less would disable.
func positive[T number](path string, v T) error {
	if v > 0 {
		return nil
	}
	return fmt.Errorf("%s %w (got %v)", path, ErrNotPositive, v)
}

// nonNegative reports path when v is below zero, for a field whose zero means
// unset, unlimited or off.
func nonNegative[T number](path string, v T) error {
	if v >= 0 {
		return nil
	}
	return fmt.Errorf("%s %w (got %v)", path, ErrNegative, v)
}

// between reports path unless v lies within lo and hi, both included.
func between[T number](path string, v, lo, hi T) error {
	if v >= lo && v <= hi {
		return nil
	}
	return fmt.Errorf("%s %w: want %v to %v (got %v)", path, ErrOutOfRange, lo, hi, v)
}

// atLeast reports path when v is below lo.
func atLeast[T number](path string, v, lo T) error {
	if v >= lo {
		return nil
	}
	return fmt.Errorf("%s %w: want at least %v (got %v)", path, ErrOutOfRange, lo, v)
}

// failed keeps the checks that failed, so a section lists its field checks in
// one call and adds only the real errors to what it reports.
func failed(checks ...error) []error {
	var errs []error
	for _, err := range checks {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// prefixed wraps a sentinel with a positional prefix like "buckets[3]" or
// "backends[0]" so error chains carry both the sentinel (for errors.Is)
// and human context (for the Error() string).
func prefixed(prefix string, sentinel error) error {
	return fmt.Errorf("%s: %w", prefix, sentinel)
}

// prefixedDetail is prefixed() with an additional free-form suffix useful
// when the sentinel alone doesn't identify the offending value (e.g. "got
// %d bytes" after ErrInvalidKeyLength).
func prefixedDetail(prefix string, sentinel error, detail string) error {
	return fmt.Errorf("%s: %w: %s", prefix, sentinel, detail)
}

// wrappedPath formats a loader error as `<sentinel> "<path>": <cause>`, keeping
// both the sentinel and the cause reachable via errors.Is.
func wrappedPath(sentinel error, path string, cause error) error {
	return fmt.Errorf("%w %q: %w", sentinel, path, cause)
}

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// The field checks' sentinels. Each is wrapped with the offending field's path,
// as in "backends[0].endpoint is required".
var (
	ErrRequired    = errors.New("is required")
	ErrNotPositive = errors.New("must be positive")
	ErrNegative    = errors.New("must not be negative")
	ErrOutOfRange  = errors.New("is out of range")
)

// Loader-level wrapping errors (LoadConfig).
var (
	ErrReadConfigFile = errors.New("read config file")
	ErrParseConfig    = errors.New("parse config")
	ErrInvalidConfig  = errors.New("invalid config")
)

// Bucket validation errors.
//
// A duplicate proxy token is refused because the token selects the bucket a
// request is authorized against: sharing one would resolve to whichever bucket
// the registry happened to store last.
var (
	ErrBucketNameRequired  = errors.New("bucket name is required")
	ErrBucketNameHasSlash  = errors.New("bucket name must not contain '/'")
	ErrDuplicateBucketName = errors.New("duplicate bucket name")
	ErrNoCredential        = errors.New("at least one credential is required")
	ErrInvalidCredential   = errors.New("must have both access_key_id and secret_access_key")
	ErrDuplicateCredential = errors.New("duplicate access_key_id")
	ErrMaxUploadsRange     = errors.New("max_multipart_uploads must be between 0 and 2147483647")
)

// Bucket CORS validation errors. An origin pattern accepts at most one '*'
// because the matcher splits on it, and a second wildcard has no unambiguous
// reading.
var (
	ErrCORSNoOrigins      = errors.New("cors rule requires at least one allowed_origins entry")
	ErrCORSNoMethods      = errors.New("cors rule requires at least one allowed_methods entry")
	ErrCORSEmptyOrigin    = errors.New("cors allowed_origins entry must not be empty")
	ErrCORSOriginWildcard = errors.New("cors allowed_origins entry accepts at most one '*'")
	ErrCORSBadMethod      = errors.New("cors allowed_methods entry is not a supported method")
	ErrCORSNegativeMaxAge = errors.New("cors max_age must be >= 0")
)

// Backend validation errors.
//
// api_request_limit is the single-pool spelling of request_limits, so setting
// both is ambiguous rather than additive. credential_source accepts "static"
// or "default_chain", and the chain form refuses static keys alongside it.
var (
	ErrDuplicateBackend      = errors.New("duplicate backend name")
	ErrUsageResetTimezone    = errors.New("usage_reset_timezone is not a known time zone")
	ErrUsageResetPartialHour = errors.New("usage_reset_timezone must be a whole number of hours from UTC")
	ErrDuplicatePoolName     = errors.New("duplicate request_limits pool name")
	ErrUnknownOperation      = errors.New("unknown operation name")
	ErrUnmeteredWildcard     = errors.New(`unmetered does not accept "*"`)
	ErrPoolChargesUnmetered  = errors.New("request_limits pool charges an operation listed as unmetered")

	ErrPoolsWithAPILimit           = errors.New("api_request_limit and request_limits are mutually exclusive")
	ErrInvalidCredentialSource     = errors.New(`credential_source must be "static" or "default_chain"`)
	ErrCredentialsWithDefaultChain = errors.New("access_key_id/secret_access_key must be empty when credential_source is default_chain")
)

// Server and TLS errors.
var (
	ErrInvalidTLSConfig  = errors.New("both cert_file and key_file are required for TLS")
	ErrInvalidTLSVersion = errors.New("invalid TLS min_version")
	ErrInvalidLogLevel   = errors.New("invalid log_level")
	ErrSpillDirUnusable  = errors.New("server.spill_dir is not a usable directory")
)

// UI / auth errors.
var (
	ErrRootCredentialIncomplete = errors.New(
		"auth.root.access_key_id and auth.root.secret_access_key must both be set (or both empty)")

	ErrRootCredentialRequired = errors.New(
		"auth.root is required when the dashboard is enabled: it is the credential " +
			"a deployment administers itself with, and without it nothing can log in " +
			"or provision the first user")

	ErrRemovedAdminToken = errors.New(
		"ui.admin_token has been removed: declare auth.root and sign admin " +
			"requests with that keypair")

	ErrRemovedAdminLogin = errors.New(
		"ui.admin_key and ui.admin_secret have been removed: the dashboard logs " +
			"in with auth.root or any provisioned credential")

	ErrRemovedProxyToken = errors.New(
		"credential token has been removed: give the credential an " +
			"access_key_id and secret_access_key instead")
)

// Rate limit errors.
var ErrInvalidCIDR = errors.New("invalid CIDR in trusted_proxies")

// Encryption errors.
var (
	ErrChunkSizeNotPowerOf2 = errors.New("encryption.chunk_size must be a power of 2")
	ErrKeySourceRequired    = errors.New("encryption: exactly one of master_key, master_key_file, or vault is required")
	ErrMultipleKeySources   = errors.New("encryption: only one of master_key, master_key_file, or vault may be set")
	ErrInvalidBase64Key     = errors.New("invalid base64 key material")
	ErrInvalidKeyLength     = errors.New("key must be 256 bits (32 bytes)")
	ErrInvalidKeyFile       = errors.New("unreadable key file")
	ErrPreviousKeyInvalid   = errors.New("previous key invalid")
	ErrVaultTokenRequired   = errors.New("encryption.vault: one of token or token_file is required")
	ErrVaultTokenAmbiguous  = errors.New("encryption.vault: only one of token or token_file may be set")
)

// Compression errors. The ratio's lower bound is excluded, which the shared
// range check does not express.
var (
	ErrInvalidCompressionLevel    = errors.New(`compression.level must be one of "fastest", "default", "better", "best"`)
	ErrInvalidCompressionMinRatio = errors.New("compression.min_ratio must be > 0 and <= 1")
)

// Lifecycle errors.
var (
	ErrLifecycleFilterRequired = errors.New("lifecycle.rules: at least one of prefix or tags must be set (an unfiltered rule would match all objects)")
	ErrLifecycleEmptyTagKey    = errors.New("lifecycle.rules: tag key must not be empty")
	ErrDuplicateFilter         = errors.New("duplicate filter")
)

// Routing, rebalance and replication errors. Both strategy fields take a
// RoutingStrategy, so they share one sentinel.
var (
	ErrInvalidRoutingStrategy    = errors.New("strategy must be 'pack' or 'spread'")
	ErrReplicationFactorTooLarge = errors.New("replication.factor exceeds number of backends")
	ErrQuotaMixNotAllowed        = errors.New("cannot mix unlimited (quota_bytes: 0) and quota-limited backends")
	ErrUnlimitedNeedsReplication = errors.New("multiple backends with unlimited quota require replication.factor >= 2")
	ErrParallelCopiesOverFactor  = errors.New("write_path.parallel_copies.count exceeds replication.factor")
)

// Usage flush errors. The threshold excludes both ends, which the shared range
// check does not express.
var (
	ErrUsageFlushFastInterval   = errors.New("usage_flush.fast_interval must be less than interval")
	ErrUsageFlushThresholdRange = errors.New("usage_flush.adaptive_threshold must be between 0 and 1")
)

// Cache errors.
var ErrCacheMaxObjectExceedsMaxSize = errors.New("cache.max_object_size cannot exceed cache.max_size")
