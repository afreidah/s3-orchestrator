// -------------------------------------------------------------------------------
// Compression Configuration
//
// Author: Alex Freidah
//
// Defines CompressionConfig: whether objects are stored compressed, at which
// zstd level, in how large a chunk, and the two thresholds that decide an
// object is not worth compressing - the size below which it is skipped
// outright, and the ratio below which an encoded object is discarded for the
// original. Validation runs at startup so a bad level or an out-of-range chunk
// size fails the process rather than the first PUT.
//
// The level is a name rather than a number because zstd collapses the numeric
// 1-19 range into four buckets, so numbers 10 and 19 emit byte-identical
// output. Names expose exactly the granularity the encoder implements.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"slices"
)

// Compression defaults and chunk size bounds. The chunk default keeps the
// ratio cost of independent frames negligible while a range read still fetches
// a small part of the object. The minimum ratio asks for a 5% saving, since
// less does not pay for the decode on every later read.
const (
	DefaultCompressionLevel     = "default"
	DefaultCompressionChunkSize = 1 << 20 // 1 MiB
	MinCompressionChunkSize     = 1 << 14 // 16 KiB
	MaxCompressionChunkSize     = 1 << 26 // 64 MiB
	DefaultCompressionMinSize   = 4096
	DefaultCompressionMinRatio  = 0.95
)

// compressionLevels are the four levels the zstd encoder distinguishes,
// named as the encoder itself names them.
var compressionLevels = []string{"fastest", "default", "better", "best"}

// CompressionConfig holds settings for at-rest compression. When enabled,
// objects are stored as chunked zstd and served back as the bytes the client
// wrote; sizes, ETags and content hashes stay those of the logical object.
type CompressionConfig struct {
	Enabled   bool    `yaml:"enabled"`    // Compress objects before storing (default: false)
	Level     string  `yaml:"level"`      // fastest, default, better, or best (default: "default")
	ChunkSize int     `yaml:"chunk_size"` // Logical bytes per independently decodable frame (default: 1048576, range: 16KB-64MB)
	MinSize   int64   `yaml:"min_size"`   // Objects smaller than this are stored uncompressed (default: 4096)
	MinRatio  float64 `yaml:"min_ratio"`  // Encoded/original size an object must reach to be stored compressed (default: 0.95)
}

// setDefaultsAndValidate applies defaults and checks the level, chunk size,
// minimum size and minimum ratio.
// Defaults apply even when compression is disabled, because the read-path codec
// and compress-existing still use them; a zero minimum ratio would make
// compress-existing decline every object. Validation runs only when enabled, so
// a half-filled block on a disabled feature does not fail startup.
func (c *CompressionConfig) setDefaultsAndValidate() []error {
	c.Level = cmp.Or(c.Level, DefaultCompressionLevel)
	c.ChunkSize = cmp.Or(c.ChunkSize, DefaultCompressionChunkSize)
	c.MinSize = cmp.Or(c.MinSize, DefaultCompressionMinSize)
	c.MinRatio = cmp.Or(c.MinRatio, DefaultCompressionMinRatio)

	if !c.Enabled {
		return nil
	}

	var errs []error
	if !slices.Contains(compressionLevels, c.Level) {
		errs = append(errs, ErrInvalidCompressionLevel)
	}
	if c.ChunkSize < MinCompressionChunkSize || c.ChunkSize > MaxCompressionChunkSize {
		errs = append(errs, ErrInvalidCompressionChunkSize)
	}
	if c.MinSize < 0 {
		errs = append(errs, ErrInvalidCompressionMinSize)
	}
	// Above 1 would store an object the encoder made larger; at or below 0 no
	// object could ever qualify, silently disabling the feature.
	if c.MinRatio <= 0 || c.MinRatio > 1 {
		errs = append(errs, ErrInvalidCompressionMinRatio)
	}
	return errs
}
