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

// Compression defaults and chunk size bounds. The chunk default is the point
// where the ratio cost of splitting an object into independently decodable
// frames is negligible while a range read still fetches a small part of the
// object; the bounds keep the seek table from dwarfing a small object at one
// end and a single-chunk read from pulling an unreasonable amount at the
// other. The minimum size default keeps the per-object frame and seek-table
// overhead from exceeding what compressing a tiny object could save. The
// minimum ratio default asks for a 5% saving before an object is stored
// encoded, since anything less does not pay for the decode every later read of
// it performs.
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
//
// The defaults apply whether or not compression is enabled, because things
// other than the write path read them: the codec is built either way so stored
// objects stay readable, and compress-existing is a legitimate thing to run on
// a fleet that has not turned the feature on for writes yet. Leaving the zero
// values in place there made that pass decline every object, since no encoding
// can beat a minimum ratio of zero.
//
// Validation is what stays gated: a half-filled block on a disabled feature
// must not fail startup.
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
