// -------------------------------------------------------------------------------
// Bucket Configuration
//
// Author: Alex Freidah
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
	AccessKeyID    string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	Token          string `yaml:"token"`
}

// BucketConfig defines a virtual bucket with one or more credential sets.
// Multiple services can share a bucket by each having their own credentials.
type BucketConfig struct {
	Name                string             `yaml:"name"`
	Credentials         []CredentialConfig `yaml:"credentials"`
	MaxMultipartUploads int                `yaml:"max_multipart_uploads"` // Max active multipart uploads per bucket (0 = unlimited)
}

func validateBuckets(buckets []BucketConfig) []string {
	var errs []string

	if len(buckets) == 0 {
		errs = append(errs, "at least one bucket is required")
	}

	bucketNames := make(map[string]bool)
	accessKeys := make(map[string]bool)
	for i := range buckets {
		bkt := &buckets[i]
		prefix := fmt.Sprintf("buckets[%d]", i)

		if bkt.Name == "" {
			errs = append(errs, fmt.Sprintf("%s: name is required", prefix))
		}
		if strings.Contains(bkt.Name, "/") {
			errs = append(errs, fmt.Sprintf("%s: name must not contain '/'", prefix))
		}
		if bucketNames[bkt.Name] {
			errs = append(errs, fmt.Sprintf("%s: duplicate bucket name '%s'", prefix, bkt.Name))
		}
		bucketNames[bkt.Name] = true

		if bkt.MaxMultipartUploads < 0 {
			errs = append(errs, fmt.Sprintf("%s: max_multipart_uploads must be >= 0", prefix))
		}

		if len(bkt.Credentials) == 0 {
			errs = append(errs, fmt.Sprintf("%s: at least one credential is required", prefix))
		}

		for j := range bkt.Credentials {
			cred := &bkt.Credentials[j]
			credPrefix := fmt.Sprintf("%s.credentials[%d]", prefix, j)

			hasSigV4 := cred.AccessKeyID != "" && cred.SecretAccessKey != ""
			hasToken := cred.Token != ""
			if !hasSigV4 && !hasToken {
				errs = append(errs, fmt.Sprintf("%s: must have access_key_id+secret_access_key or token", credPrefix))
			}

			if cred.AccessKeyID != "" {
				if accessKeys[cred.AccessKeyID] {
					errs = append(errs, fmt.Sprintf("%s: duplicate access_key_id '%s'", credPrefix, cred.AccessKeyID))
				}
				accessKeys[cred.AccessKeyID] = true
			}
		}
	}

	return errs
}
