// -------------------------------------------------------------------------------
// Backend Configuration
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package config

import "fmt"

// BackendConfig holds configuration for an S3-compatible storage backend.
type BackendConfig struct {
	Name            string `yaml:"name"`              // Identifier for metrics/tracing
	Endpoint        string `yaml:"endpoint"`          // S3-compatible endpoint URL
	Region          string `yaml:"region"`            // AWS region or equivalent
	Bucket          string `yaml:"bucket"`            // Target bucket name
	AccessKeyID     string `yaml:"access_key_id"`     // AWS access key ID
	SecretAccessKey string `yaml:"secret_access_key"` // AWS secret access key
	ForcePathStyle   bool  `yaml:"force_path_style"`   // Use path-style URLs
	UnsignedPayload  *bool `yaml:"unsigned_payload"`   // Skip SigV4 payload hash to stream uploads without buffering (default: true)
	DisableChecksum  bool  `yaml:"disable_checksum"`   // Disable SDK default checksums for GCS and other providers that reject them (default: false)
	StripSDKHeaders  bool  `yaml:"strip_sdk_headers"`  // Remove SDK v2 headers (amz-sdk-*, accept-encoding, x-id) before signing for GCS compatibility (default: false)
	QuotaBytes       int64 `yaml:"quota_bytes"`        // Maximum bytes allowed on this backend (0 = unlimited)
	APIRequestLimit  int64 `yaml:"api_request_limit"`  // Monthly API request limit (0 = unlimited)
	EgressByteLimit  int64 `yaml:"egress_byte_limit"`  // Monthly egress byte limit (0 = unlimited)
	IngressByteLimit int64 `yaml:"ingress_byte_limit"` // Monthly ingress byte limit (0 = unlimited)
}

func validateBackends(backends []BackendConfig) []string {
	var errs []string

	if len(backends) == 0 {
		errs = append(errs, "at least one backend is required")
	}

	names := make(map[string]bool)
	for i := range backends {
		b := &backends[i]
		prefix := fmt.Sprintf("backends[%d]", i)

		if b.Name == "" {
			b.Name = fmt.Sprintf("backend-%d", i)
		}
		if names[b.Name] {
			errs = append(errs, fmt.Sprintf("%s: duplicate backend name '%s'", prefix, b.Name))
		}
		names[b.Name] = true

		if b.Endpoint == "" {
			errs = append(errs, fmt.Sprintf("%s: endpoint is required", prefix))
		}
		if b.Bucket == "" {
			errs = append(errs, fmt.Sprintf("%s: bucket is required", prefix))
		}
		if b.AccessKeyID == "" {
			errs = append(errs, fmt.Sprintf("%s: access_key_id is required", prefix))
		}
		if b.SecretAccessKey == "" {
			errs = append(errs, fmt.Sprintf("%s: secret_access_key is required", prefix))
		}
		if b.QuotaBytes < 0 {
			errs = append(errs, fmt.Sprintf("%s: quota_bytes must not be negative", prefix))
		}
		if b.APIRequestLimit < 0 {
			errs = append(errs, fmt.Sprintf("%s: api_request_limit must not be negative", prefix))
		}
		if b.EgressByteLimit < 0 {
			errs = append(errs, fmt.Sprintf("%s: egress_byte_limit must not be negative", prefix))
		}
		if b.IngressByteLimit < 0 {
			errs = append(errs, fmt.Sprintf("%s: ingress_byte_limit must not be negative", prefix))
		}
	}

	return errs
}
