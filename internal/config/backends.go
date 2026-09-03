// -------------------------------------------------------------------------------
// Backend Configuration
//
// Author: Alex Freidah
//
// Defines BackendConfig - the per-backend YAML block describing the S3
// endpoint, credentials, optional quota and per-object size cap, and the
// read/write tunables (timeouts, max idle conns, signing tweaks) - plus
// its validators. Every backend listed in config.yaml is parsed into one
// of these structs and handed to the backend runtime at startup.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// CredentialSourceStatic and friends enumerate the supported credential_source values.
const (
	CredentialSourceStatic       = "static"        // access_key_id / secret_access_key from the config
	CredentialSourceDefaultChain = "default_chain" // AWS SDK default chain: env, IMDS, SSO, ~/.aws, STS
)

// BackendConfig holds configuration for an S3-compatible storage backend.
type BackendConfig struct {
	Name             string `yaml:"name"`               // Identifier for metrics/tracing
	Endpoint         string `yaml:"endpoint"`           // S3-compatible endpoint URL
	Region           string `yaml:"region"`             // AWS region or equivalent
	Bucket           string `yaml:"bucket"`             // Target bucket name
	AccessKeyID      string `yaml:"access_key_id"`      // AWS access key ID (required when credential_source is "static")
	SecretAccessKey  string `yaml:"secret_access_key"`  // AWS secret access key (required when credential_source is "static")
	CredentialSource string `yaml:"credential_source"`  // "static" (default) or "default_chain": env, IMDS, SSO, STS
	ForcePathStyle   bool   `yaml:"force_path_style"`   // Use path-style URLs
	UnsignedPayload  *bool  `yaml:"unsigned_payload"`   // Skip SigV4 payload hash to stream uploads without buffering (default: true)
	DisableChecksum  bool   `yaml:"disable_checksum"`   // Disable SDK default checksums for GCS and other providers that reject them (default: false)
	StripSDKHeaders  bool   `yaml:"strip_sdk_headers"`  // Remove SDK v2 headers (amz-sdk-*, accept-encoding, x-id) before signing for GCS compatibility (default: false)
	QuotaBytes       int64  `yaml:"quota_bytes"`        // Maximum bytes allowed on this backend (0 = unlimited)
	MaxObjectSize    int64  `yaml:"max_object_size"`    // Maximum size of a single object in bytes (0 = unlimited)
	APIRequestLimit  int64  `yaml:"api_request_limit"`  // Monthly API request limit (0 = unlimited)
	EgressByteLimit  int64  `yaml:"egress_byte_limit"`  // Monthly egress byte limit (0 = unlimited)
	IngressByteLimit int64  `yaml:"ingress_byte_limit"` // Monthly ingress byte limit (0 = unlimited)

	RequestLimits []RequestPoolConfig `yaml:"request_limits"` // Per-operation request budgets (see RequestPoolConfig)
	Unmetered     []string            `yaml:"unmetered"`      // Operations the provider does not bill, charged to no budget

	HTTP BackendHTTPConfig `yaml:"http"` // Per-backend HTTP transport tuning
}

// Defaults for BackendHTTPConfig, applied per backend when the block is
// omitted or a field is left at zero. Sized for a proxy serving concurrent
// client traffic alongside the rebalancer and replicator.
const (
	DefaultMaxIdleConns          = 100
	DefaultMaxIdleConnsPerHost   = 100
	DefaultMaxConnsPerHost       = 200
	DefaultResponseHeaderTimeout = 30 * time.Second
)

// BackendHTTPConfig tunes the HTTP transport of one backend. Every field is
// optional; a zero value takes the default above, so an existing config that
// never mentions the block behaves exactly as it did.
//
// The pool sizes are per backend, and deployments range from a Raspberry Pi to
// a high-concurrency gateway: one fixed setting either over-allocates file
// descriptors on a small box or starves throughput on a large one.
//
// ForceHTTP2 is a pointer so an explicit false is distinguishable from an
// omitted field. Setting it false makes this backend negotiate HTTP/1.1, which
// is the targeted form of the GODEBUG=http2client=0 workaround: HTTP/2 against
// some proxy and gateway combinations collapses throughput by most of an order
// of magnitude, and an operator who hits that needs a way out that does not
// change how every other backend is dialled.
type BackendHTTPConfig struct {
	MaxIdleConns          int           `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost   int           `yaml:"max_idle_conns_per_host"`
	MaxConnsPerHost       int           `yaml:"max_conns_per_host"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	ForceHTTP2            *bool         `yaml:"force_http2"`
}

// HTTP2Enabled reports whether this backend should attempt HTTP/2, which it
// does unless the operator turned it off.
func (h BackendHTTPConfig) HTTP2Enabled() bool {
	return h.ForceHTTP2 == nil || *h.ForceHTTP2
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// setDefaults fills each unset field with its default. Zero means unset
// rather than "no limit": Go's transport reads zero as unlimited for some of
// these and as its own small default for others, and neither is what an
// operator who omitted the field is asking for.
func (h *BackendHTTPConfig) setDefaults() {
	h.MaxIdleConns = cmp.Or(h.MaxIdleConns, DefaultMaxIdleConns)
	h.MaxIdleConnsPerHost = cmp.Or(h.MaxIdleConnsPerHost, DefaultMaxIdleConnsPerHost)
	h.MaxConnsPerHost = cmp.Or(h.MaxConnsPerHost, DefaultMaxConnsPerHost)
	h.ResponseHeaderTimeout = cmp.Or(h.ResponseHeaderTimeout, DefaultResponseHeaderTimeout)
}

// validate rejects negative values, which no transport field accepts. Fields
// are checked in a fixed order so an operator with several typos sees them
// listed the same way every run.
func (h BackendHTTPConfig) validate(prefix string) []error {
	fields := []struct {
		name  string
		value int64
	}{
		{"http.max_idle_conns", int64(h.MaxIdleConns)},
		{"http.max_idle_conns_per_host", int64(h.MaxIdleConnsPerHost)},
		{"http.max_conns_per_host", int64(h.MaxConnsPerHost)},
		{"http.response_header_timeout", int64(h.ResponseHeaderTimeout)},
	}

	var errs []error
	for _, f := range fields {
		if f.value < 0 {
			errs = append(errs, prefixedDetail(prefix, ErrNegativeHTTPSetting, f.name))
		}
	}
	return errs
}

// validateBackends checks every BackendConfig in the configured list,
// fills missing names with backend-N defaults, and surfaces every
// per-entry problem in one pass so operators see all the typos at
// once rather than fixing one and rerunning.
func validateBackends(backends []BackendConfig) []error {
	var errs []error

	if len(backends) == 0 {
		errs = append(errs, ErrNoBackends)
	}

	seenNames := make(map[string]bool)
	for i := range backends {
		errs = append(errs, validateBackend(i, &backends[i], seenNames)...)
	}
	return errs
}

// validateBackend checks one entry in the backends list. seenNames is the
// caller's shared set used to flag name collisions across the full list.
// An empty Name is filled with a default so later messages and metrics can
// reference it unambiguously. An empty CredentialSource defaults to
// "static" so existing configs keep working without the new field.
func validateBackend(idx int, b *BackendConfig, seenNames map[string]bool) []error {
	prefix := fmt.Sprintf("backends[%d]", idx)
	if b.Name == "" {
		b.Name = fmt.Sprintf("backend-%d", idx)
	}
	b.CredentialSource = cmp.Or(b.CredentialSource, CredentialSourceStatic)

	var errs []error
	if seenNames[b.Name] {
		errs = append(errs, prefixedDetail(prefix, ErrDuplicateBackend, fmt.Sprintf("%q", b.Name)))
	}
	seenNames[b.Name] = true

	errs = append(errs, requiredBackendStringErrs(prefix, b)...)
	errs = append(errs, credentialSourceErrs(prefix, b)...)
	errs = append(errs, b.HTTP.validate(prefix)...)
	b.HTTP.setDefaults()
	errs = append(errs, nonNegativeBackendFieldErrs(prefix, b)...)
	errs = append(errs, requestLimitErrs(prefix, b)...)
	return errs
}

// requiredBackendStringErrs returns errors for the universally-required
// string fields (endpoint, bucket). Credential-related fields are
// handled by credentialSourceErrs since the requirement depends on the
// source.
func requiredBackendStringErrs(prefix string, b *BackendConfig) []error {
	var errs []error
	if b.Endpoint == "" {
		errs = append(errs, prefixed(prefix, ErrEndpointRequired))
	}
	if b.Bucket == "" {
		errs = append(errs, prefixed(prefix, ErrBackendBucketReqd))
	}
	return errs
}

// credentialSourceErrs enforces the per-source key requirements: static
// needs both keys; default_chain rejects them so a stale entry cannot
// silently shadow the SDK-resolved credentials.
func credentialSourceErrs(prefix string, b *BackendConfig) []error {
	var errs []error
	switch b.CredentialSource {
	case CredentialSourceStatic:
		if b.AccessKeyID == "" {
			errs = append(errs, prefixed(prefix, ErrAccessKeyIDReqd))
		}
		if b.SecretAccessKey == "" {
			errs = append(errs, prefixed(prefix, ErrSecretAccessKeyReqd))
		}
	case CredentialSourceDefaultChain:
		if b.AccessKeyID != "" || b.SecretAccessKey != "" {
			errs = append(errs, prefixed(prefix, ErrCredentialsWithDefaultChain))
		}
	default:
		errs = append(errs, prefixedDetail(prefix, ErrInvalidCredentialSource, fmt.Sprintf("got %q", b.CredentialSource)))
	}
	return errs
}

// RequestPoolConfig is one monthly request budget shared by a set of
// operations, which is how providers actually meter: GCS bills uploads and
// listings from one allowance and reads from a much larger separate one, and
// B2 splits them differently again. Naming the grouping in config rather than
// in code keeps the orchestrator out of the business of tracking each
// provider's price list.
//
// Pools are additive. An operation charges every pool that contains it and is
// admitted only when all of them have headroom, so a per-operation sub-cap can
// sit inside an aggregate cap. Limit 0 means unlimited: the pool is still
// counted and reported, it simply never refuses.
type RequestPoolConfig struct {
	Name       string   `yaml:"name"`       // Identifier for the counter, metric label and usage report
	Operations []string `yaml:"operations"` // Operation names, or "*" for every metered operation
	Limit      int64    `yaml:"limit"`      // Monthly ceiling shared by those operations (0 = unlimited)
}

// requestLimitErrs validates the per-backend request budgets: pool identity,
// known operation names, and the two config states that cannot be resolved in
// any one direction without guessing at intent.
func requestLimitErrs(prefix string, b *BackendConfig) []error {
	var errs []error
	if b.APIRequestLimit > 0 && len(b.RequestLimits) > 0 {
		errs = append(errs, prefixed(prefix, ErrPoolsWithAPILimit))
	}

	unmetered := make(map[string]bool, len(b.Unmetered))
	for _, name := range b.Unmetered {
		if name == s3op.Wildcard {
			errs = append(errs, prefixed(prefix, ErrUnmeteredWildcard))
			continue
		}
		if !s3op.Known(name) {
			errs = append(errs, prefixedDetail(prefix, ErrUnknownOperation, fmt.Sprintf("unmetered: %q", name)))
			continue
		}
		unmetered[name] = true
	}

	seen := make(map[string]bool, len(b.RequestLimits))
	for i := range b.RequestLimits {
		errs = append(errs, poolErrs(fmt.Sprintf("%s.request_limits[%d]", prefix, i), &b.RequestLimits[i], seen, unmetered)...)
	}
	return errs
}

// poolErrs validates one pool entry against the backend's unmetered set and
// the pool names already seen.
func poolErrs(prefix string, p *RequestPoolConfig, seen, unmetered map[string]bool) []error {
	var errs []error
	switch {
	case p.Name == "":
		errs = append(errs, prefixed(prefix, ErrPoolNameRequired))
	case seen[p.Name]:
		errs = append(errs, prefixedDetail(prefix, ErrDuplicatePoolName, fmt.Sprintf("%q", p.Name)))
	default:
		seen[p.Name] = true
	}
	if len(p.Operations) == 0 {
		errs = append(errs, prefixed(prefix, ErrPoolOperationsReqd))
	}
	if p.Limit < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativePoolLimit))
	}
	for _, name := range p.Operations {
		switch {
		case name == s3op.Wildcard:
		case !s3op.Known(name):
			errs = append(errs, prefixedDetail(prefix, ErrUnknownOperation, fmt.Sprintf("%q", name)))
		case unmetered[name]:
			errs = append(errs, prefixedDetail(prefix, ErrPoolChargesUnmetered, fmt.Sprintf("%q", name)))
		}
	}
	return errs
}

// nonNegativeBackendFieldErrs returns one error per quota/limit field that
// has gone negative.
func nonNegativeBackendFieldErrs(prefix string, b *BackendConfig) []error {
	var errs []error
	if b.QuotaBytes < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativeQuota))
	}
	if b.MaxObjectSize < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativeMaxObject))
	}
	if b.APIRequestLimit < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativeAPILimit))
	}
	if b.EgressByteLimit < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativeEgress))
	}
	if b.IngressByteLimit < 0 {
		errs = append(errs, prefixed(prefix, ErrNegativeIngress))
	}
	return errs
}
