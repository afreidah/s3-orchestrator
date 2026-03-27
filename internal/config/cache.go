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
	"fmt"
	"time"
)

// CacheConfig holds settings for the object data cache.
type CacheConfig struct {
	Enabled       bool          `yaml:"enabled"`          // Enable the object data cache (default: false)
	MaxSize       string        `yaml:"max_size"`         // Maximum total cache size (e.g., "256MB", "1GB")
	MaxObjectSize string        `yaml:"max_object_size"`  // Maximum cacheable object size (e.g., "10MB"); 0 = no limit
	TTL           time.Duration `yaml:"ttl"`              // Time before a cached entry expires (default: 5m)

	// Parsed values (not from YAML)
	MaxSizeBytes       int64 `yaml:"-"`
	MaxObjectSizeBytes int64 `yaml:"-"`
}

func (cc *CacheConfig) setDefaultsAndValidate() []string {
	if !cc.Enabled {
		return nil
	}

	var errs []string

	// Default TTL
	if cc.TTL <= 0 {
		cc.TTL = 5 * time.Minute
	}

	// Parse max_size
	if cc.MaxSize == "" {
		cc.MaxSize = "256MB"
	}
	maxSize, err := parseByteSize(cc.MaxSize)
	switch {
	case err != nil:
		errs = append(errs, fmt.Sprintf("cache.max_size: %v", err))
	case maxSize <= 0:
		errs = append(errs, "cache.max_size must be positive")
	default:
		cc.MaxSizeBytes = maxSize
	}

	// Parse max_object_size
	if cc.MaxObjectSize == "" {
		cc.MaxObjectSize = "10MB"
	}
	maxObj, err := parseByteSize(cc.MaxObjectSize)
	switch {
	case err != nil:
		errs = append(errs, fmt.Sprintf("cache.max_object_size: %v", err))
	case maxObj <= 0:
		errs = append(errs, "cache.max_object_size must be positive")
	default:
		cc.MaxObjectSizeBytes = maxObj
	}

	if cc.MaxSizeBytes > 0 && cc.MaxObjectSizeBytes > 0 && cc.MaxObjectSizeBytes > cc.MaxSizeBytes {
		errs = append(errs, "cache.max_object_size cannot exceed cache.max_size")
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
	var val int64
	for _, ch := range numStr {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("invalid byte size %q", s)
		}
		val = val*10 + int64(ch-'0')
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

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
