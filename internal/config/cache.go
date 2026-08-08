// -------------------------------------------------------------------------------
// Cache Configuration
//
// Author: Alex Freidah
//
// Configuration for the optional object data cache. When enabled, full GET
// responses are cached in memory to reduce backend API calls and egress.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"fmt"
	"math"
	"strconv"
	"time"
)

// CacheConfig holds settings for the object data cache.
type CacheConfig struct {
	Enabled       bool          `yaml:"enabled"`         // Enable the object data cache (default: false)
	MaxSize       string        `yaml:"max_size"`        // Maximum total cache size (e.g., "256MB", "1GB")
	MaxObjectSize string        `yaml:"max_object_size"` // Maximum cacheable object size (e.g., "10MB"); 0 = no limit
	TTL           time.Duration `yaml:"ttl"`             // Time before a cached entry expires (default: 5m)

	// Parsed values (not from YAML)
	MaxSizeBytes       int64 `yaml:"-"`
	MaxObjectSizeBytes int64 `yaml:"-"`
}

// setDefaultsAndValidate sets defaults and validate.
func (cc *CacheConfig) setDefaultsAndValidate() []error {
	if !cc.Enabled {
		return nil
	}

	var errs []error

	// Default TTL
	if cc.TTL <= 0 {
		cc.TTL = 5 * time.Minute
	}

	// Parse max_size
	cc.MaxSize = cmp.Or(cc.MaxSize, "256MB")
	maxSize, err := parseByteSize(cc.MaxSize)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("cache.max_size: %w", err))
	case maxSize <= 0:
		errs = append(errs, ErrCacheMaxSizeNotPositive)
	default:
		cc.MaxSizeBytes = maxSize
	}

	// Parse max_object_size
	cc.MaxObjectSize = cmp.Or(cc.MaxObjectSize, "10MB")
	maxObj, err := parseByteSize(cc.MaxObjectSize)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("cache.max_object_size: %w", err))
	case maxObj <= 0:
		errs = append(errs, ErrCacheMaxObjectNotPositive)
	default:
		cc.MaxObjectSizeBytes = maxObj
	}

	if cc.MaxSizeBytes > 0 && cc.MaxObjectSizeBytes > 0 && cc.MaxObjectSizeBytes > cc.MaxSizeBytes {
		errs = append(errs, ErrCacheMaxObjectExceedsMaxSize)
	}

	return errs
}

// parseByteSize parses a human-readable byte size string like "256MB", "1GB",
// "512KB". Supports KB, MB, GB suffixes (case-insensitive). Plain integers
// are treated as bytes.
func parseByteSize(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	// Try to find a unit suffix
	s = trimSpace(s)
	var multiplier int64 = 1
	var numStr string

	upper := toUpper(s)
	switch {
	case hasSuffix(upper, "GB"):
		multiplier = 1024 * 1024 * 1024
		numStr = s[:len(s)-2]
	case hasSuffix(upper, "MB"):
		multiplier = 1024 * 1024
		numStr = s[:len(s)-2]
	case hasSuffix(upper, "KB"):
		multiplier = 1024
		numStr = s[:len(s)-2]
	case hasSuffix(upper, "B"):
		numStr = s[:len(s)-1]
	default:
		numStr = s
	}

	numStr = trimSpace(numStr)
	if numStr == "" {
		return 0, fmt.Errorf("invalid byte size %q: missing numeric value", s)
	}
	val, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q: %w", s, err)
	}
	if val < 0 {
		return 0, fmt.Errorf("invalid byte size %q: value must be non-negative", s)
	}

	// Check for overflow before applying the multiplier.
	if multiplier > 1 && val > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("byte size %q overflows int64", s)
	}
	return val * multiplier, nil
}

// trimSpace, toUpper, hasSuffix avoid importing strings for a config package.
func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// toUpper is an ASCII-only uppercase. Used for the cache mode comparator
// without pulling in unicode/text since cache mode values are a small
// closed set ("memory", "redis", etc.) and never carry non-ASCII.
func toUpper(s string) string {
	b := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

// hasSuffix reports whether suffix.
func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
