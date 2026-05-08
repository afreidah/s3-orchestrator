// -------------------------------------------------------------------------------
// Config Validation Sentinel Errors
//
// Author: Alex Freidah
//
// Config validators previously returned errors as joined strings, forcing
// tests to match on substrings. That made error messages de-facto APIs  - 
// a harmless reword would silently break test assertions.
//
// Each validator now wraps a sentinel defined below using fmt.Errorf("%w:
// <detail>", ErrX), and tests check with errors.Is(err, ErrX). Rewording the
// detail suffix is free; only the sentinel value is the contract.
// -------------------------------------------------------------------------------

package config

import (
	"errors"
	"fmt"
)

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

// wrappedPath composes a loader sentinel with the offending file path
// and the underlying cause. Used by LoadConfig so every loader error
// surfaces the same shape: "<sentinel> "<path>": <cause>". Both the
// sentinel and the cause stay reachable via errors.Is.
func wrappedPath(sentinel error, path string, cause error) error {
	return fmt.Errorf("%w %q: %w", sentinel, path, cause)
}

// Loader-level wrapping errors (LoadConfig).
var (
	ErrReadConfigFile = errors.New("read config file")
	ErrParseConfig    = errors.New("parse config")
	ErrInvalidConfig  = errors.New("invalid config")
)

// Bucket validation errors.
var (
	ErrNoBuckets           = errors.New("at least one bucket is required")
	ErrBucketNameRequired  = errors.New("bucket name is required")
	ErrBucketNameHasSlash  = errors.New("bucket name must not contain '/'")
	ErrDuplicateBucketName = errors.New("duplicate bucket name")
	ErrNoCredential        = errors.New("at least one credential is required")
	ErrInvalidCredential   = errors.New("must have access_key_id+secret_access_key or token")
	ErrDuplicateCredential = errors.New("duplicate access_key_id")
	ErrNegativeMaxUploads  = errors.New("max_multipart_uploads must be >= 0")
)

// Backend validation errors.
var (
	ErrNoBackends          = errors.New("at least one backend is required")
	ErrDuplicateBackend    = errors.New("duplicate backend name")
	ErrEndpointRequired    = errors.New("endpoint is required")
	ErrBackendBucketReqd   = errors.New("bucket is required")
	ErrAccessKeyIDReqd     = errors.New("access_key_id is required")
	ErrSecretAccessKeyReqd = errors.New("secret_access_key is required")
	ErrNegativeQuota       = errors.New("quota_bytes must not be negative")
	ErrNegativeMaxObject   = errors.New("max_object_size must not be negative")
	ErrNegativeAPILimit    = errors.New("api_request_limit must not be negative")
	ErrNegativeEgress      = errors.New("egress_byte_limit must not be negative")
	ErrNegativeIngress     = errors.New("ingress_byte_limit must not be negative")
)

// Server and TLS errors.
var (
	ErrInvalidTLSConfig  = errors.New("both cert_file and key_file are required for TLS")
	ErrInvalidTLSVersion = errors.New("invalid TLS min_version")
	ErrInvalidLogLevel   = errors.New("invalid log_level")
)

// UI / auth errors.
var (
	ErrAdminAuthIncomplete = errors.New("admin_key and admin_secret must both be set (or both empty)")
	ErrSessionSecretReqd   = errors.New("session_secret is required when UI is enabled")
)

// Rate limit errors.
var (
	ErrInvalidCIDR                 = errors.New("invalid CIDR in trusted_proxies")
	ErrRateLimitRPSNotPositive     = errors.New("rate_limit.requests_per_sec must be positive")
	ErrRateLimitBurstNotPositive   = errors.New("rate_limit.burst must be positive")
)

// Encryption errors.
var (
	ErrInvalidChunkSize         = errors.New("encryption.chunk_size must be between 4096 (4KB) and 1048576 (1MB)")
	ErrChunkSizeNotPowerOf2     = errors.New("encryption.chunk_size must be a power of 2")
	ErrKeySourceRequired        = errors.New("encryption: exactly one of master_key, master_key_file, or vault is required")
	ErrMultipleKeySources       = errors.New("encryption: only one of master_key, master_key_file, or vault may be set")
	ErrInvalidBase64Key         = errors.New("invalid base64 key material")
	ErrInvalidKeyLength         = errors.New("key must be 256 bits (32 bytes)")
	ErrInvalidKeyFile           = errors.New("invalid master_key_file")
	ErrPreviousKeyInvalid       = errors.New("previous_keys entry invalid")
	ErrVaultAddressRequired     = errors.New("encryption.vault.address is required")
	ErrVaultTokenRequired       = errors.New("encryption.vault: one of token or token_file is required")
	ErrVaultTokenAmbiguous      = errors.New("encryption.vault: only one of token or token_file may be set")
	ErrVaultKeyNameRequired     = errors.New("encryption.vault.key_name is required")
)

// Redis / counter errors.
var (
	ErrRedisAddressRequired        = errors.New("redis.address is required when redis section is present")
	ErrRedisFailureThresholdNotPos = errors.New("redis.failure_threshold must be positive")
	ErrRedisOpenTimeoutNotPos      = errors.New("redis.open_timeout must be positive")
)

// Notifications errors.
var (
	ErrNotificationURLRequired    = errors.New("notifications endpoint url is required")
	ErrNotificationEventsRequired = errors.New("notifications endpoint requires at least one event pattern")
)

// Lifecycle errors.
var (
	ErrLifecyclePrefixRequired = errors.New("lifecycle.rules: prefix must not be empty (would match all objects)")
	ErrInvalidExpiration       = errors.New("lifecycle.rules: expiration_days must be positive")
	ErrDuplicatePrefix         = errors.New("duplicate prefix")
)

// Rebalance / replication errors.
var (
	ErrInvalidRebalanceStrategy  = errors.New("rebalance.strategy must be 'pack' or 'spread'")
	ErrRebalanceIntervalNotPos   = errors.New("rebalance.interval must be positive")
	ErrRebalanceBatchNotPos      = errors.New("rebalance.batch_size must be positive")
	ErrRebalanceThresholdRange   = errors.New("rebalance.threshold must be between 0 and 1")
	ErrRebalanceConcurrencyNotPos = errors.New("rebalance.concurrency must be positive")
	ErrReplicationFactorMin      = errors.New("replication.factor must be at least 1")
	ErrReplicationFactorTooLarge = errors.New("replication.factor exceeds number of backends")
	ErrReplicationIntervalNotPos = errors.New("replication.worker_interval must be positive")
	ErrReplicationBatchNotPos    = errors.New("replication.batch_size must be positive")
	ErrInvalidRoutingStrategy    = errors.New("routing_strategy must be 'pack' or 'spread'")
	ErrQuotaMixNotAllowed        = errors.New("cannot mix unlimited (quota_bytes: 0) and quota-limited backends")
	ErrUnlimitedNeedsReplication = errors.New("multiple backends with unlimited quota require replication.factor >= 2")
)

// Usage flush / adaptive errors.
var (
	ErrUsageFlushFastInterval    = errors.New("usage_flush.fast_interval must be less than interval")
	ErrUsageFlushThresholdRange  = errors.New("usage_flush.adaptive_threshold must be between 0 and 1")
)

// Cache / storage misc.
var (
	ErrCacheMaxSizeNotPositive      = errors.New("cache.max_size must be positive")
	ErrCacheMaxObjectNotPositive    = errors.New("cache.max_object_size must be positive")
	ErrCacheMaxObjectExceedsMaxSize = errors.New("cache.max_object_size cannot exceed cache.max_size")
)

// Telemetry errors.
var (
	ErrTracingEndpointRequired = errors.New("telemetry.tracing.endpoint is required when tracing is enabled")
)