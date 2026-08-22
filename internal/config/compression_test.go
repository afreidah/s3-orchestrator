// -------------------------------------------------------------------------------
// Compression Configuration Tests
//
// Author: Alex Freidah
//
// Covers defaults, level and chunk-size validation, and the restart-required
// classification. Two tests pin the config surface to the packages that own
// the underlying values: the chunk bounds to the codec, and the level names to
// the zstd encoder. Config deliberately does not import either at runtime, so
// nothing but these tests keeps the two copies of those values honest.
// -------------------------------------------------------------------------------

package config

import (
	"errors"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/afreidah/s3-orchestrator/internal/compression"
)

// TestCompressionConfig_DisabledSkipsValidation verifies a disabled block is
// left alone, so a half-filled section cannot fail startup.
func TestCompressionConfig_DisabledSkipsValidation(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Compression = CompressionConfig{Level: "nonsense", ChunkSize: 7}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Errorf("disabled compression should skip validation: %v", err)
	}
	if cfg.Compression.Level != "nonsense" {
		t.Errorf("Level = %q, want the unvalidated value left in place", cfg.Compression.Level)
	}
}

// TestCompressionConfig_Defaults verifies the documented defaults land when
// only enabled is set.
func TestCompressionConfig_Defaults(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Compression = CompressionConfig{Enabled: true}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("enabled compression with no other fields should pass: %v", err)
	}
	if cfg.Compression.Level != "default" {
		t.Errorf("Level = %q, want %q", cfg.Compression.Level, "default")
	}
	if cfg.Compression.ChunkSize != 1<<20 {
		t.Errorf("ChunkSize = %d, want %d", cfg.Compression.ChunkSize, 1<<20)
	}
	if cfg.Compression.MinSize != 4096 {
		t.Errorf("MinSize = %d, want 4096", cfg.Compression.MinSize)
	}
	if cfg.Compression.MinRatio != DefaultCompressionMinRatio {
		t.Errorf("MinRatio = %v, want %v", cfg.Compression.MinRatio, DefaultCompressionMinRatio)
	}
}

// TestCompressionConfig_MinRatioBounds verifies the ratio is held to (0, 1].
// An unset field takes the default rather than the zero value, which would
// otherwise disable compression by making no object qualify.
func TestCompressionConfig_MinRatioBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		minRatio float64
		wantErr  bool
	}{
		{"unset takes the default", 0, false},
		{"negative", -0.5, true},
		{"a real saving", 0.5, false},
		{"any saving at all", 1, false},
		{"larger than the original", 1.01, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validBaseConfig()
			cfg.Compression = CompressionConfig{Enabled: true, MinRatio: tt.minRatio}

			err := cfg.SetDefaultsAndValidate()
			if tt.wantErr && !errors.Is(err, ErrInvalidCompressionMinRatio) {
				t.Errorf("min_ratio %v: want ErrInvalidCompressionMinRatio, got %v", tt.minRatio, err)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("min_ratio %v: want no error, got %v", tt.minRatio, err)
			}
		})
	}
}

// TestCompressionConfig_AcceptsEveryLevel verifies each supported name passes
// validation.
func TestCompressionConfig_AcceptsEveryLevel(t *testing.T) {
	t.Parallel()
	for _, level := range compressionLevels {
		t.Run(level, func(t *testing.T) {
			t.Parallel()
			cfg := validBaseConfig()
			cfg.Compression = CompressionConfig{Enabled: true, Level: level}

			if err := cfg.SetDefaultsAndValidate(); err != nil {
				t.Errorf("level %q should pass: %v", level, err)
			}
		})
	}
}

// TestCompressionConfig_RejectsNumericLevel verifies a zstd-style number is
// refused. Accepting one would imply granularity the encoder does not have,
// which is the reason the surface is named rather than numeric.
func TestCompressionConfig_RejectsNumericLevel(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Compression = CompressionConfig{Enabled: true, Level: "19"}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !errors.Is(err, ErrInvalidCompressionLevel) {
		t.Errorf("numeric level should fail with ErrInvalidCompressionLevel, got: %v", err)
	}
}

// TestCompressionConfig_ChunkSizeBounds verifies both ends of the range are
// enforced and that the boundary values themselves are accepted.
func TestCompressionConfig_ChunkSizeBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		chunkSize int
		wantErr   bool
	}{
		{"below minimum", MinCompressionChunkSize - 1, true},
		{"at minimum", MinCompressionChunkSize, false},
		{"at maximum", MaxCompressionChunkSize, false},
		{"above maximum", MaxCompressionChunkSize + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validBaseConfig()
			cfg.Compression = CompressionConfig{Enabled: true, ChunkSize: tt.chunkSize}

			err := cfg.SetDefaultsAndValidate()
			if tt.wantErr && !errors.Is(err, ErrInvalidCompressionChunkSize) {
				t.Errorf("chunk size %d: want ErrInvalidCompressionChunkSize, got %v", tt.chunkSize, err)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("chunk size %d: want no error, got %v", tt.chunkSize, err)
			}
		})
	}
}

// TestCompressionConfig_NegativeMinSize verifies a negative threshold is
// refused rather than silently treated as zero.
func TestCompressionConfig_NegativeMinSize(t *testing.T) {
	t.Parallel()
	cfg := validBaseConfig()
	cfg.Compression = CompressionConfig{Enabled: true, MinSize: -1}

	err := cfg.SetDefaultsAndValidate()
	if err == nil || !errors.Is(err, ErrInvalidCompressionMinSize) {
		t.Errorf("negative min_size should fail with ErrInvalidCompressionMinSize, got: %v", err)
	}
}

// TestCompressionConfig_RequiresRestart verifies every field is reported as
// restart-required, and that an unchanged block reports nothing.
func TestCompressionConfig_RequiresRestart(t *testing.T) {
	t.Parallel()
	base := CompressionConfig{Enabled: true, Level: "default", ChunkSize: 1 << 20, MinSize: 4096, MinRatio: 0.95}
	tests := []struct {
		name string
		next CompressionConfig
		want bool
	}{
		{"unchanged", base, false},
		{"enabled", CompressionConfig{Enabled: false, Level: "default", ChunkSize: 1 << 20, MinSize: 4096, MinRatio: 0.95}, true},
		{"level", CompressionConfig{Enabled: true, Level: "best", ChunkSize: 1 << 20, MinSize: 4096, MinRatio: 0.95}, true},
		{"chunk size", CompressionConfig{Enabled: true, Level: "default", ChunkSize: 1 << 21, MinSize: 4096, MinRatio: 0.95}, true},
		{"min size", CompressionConfig{Enabled: true, Level: "default", ChunkSize: 1 << 20, MinSize: 8192, MinRatio: 0.95}, true},
		{"min ratio", CompressionConfig{Enabled: true, Level: "default", ChunkSize: 1 << 20, MinSize: 4096, MinRatio: 0.5}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			old := validBaseConfig()
			old.Compression = base
			next := validBaseConfig()
			next.Compression = tt.next

			var found bool
			for _, field := range NonReloadableFieldsChanged(&old, &next) {
				if field == "compression" {
					found = true
				}
			}
			if found != tt.want {
				t.Errorf("compression reported restart-required = %v, want %v", found, tt.want)
			}
		})
	}
}

// TestCompressionConfig_ChunkBoundsMatchCodec pins the config bounds to the
// codec that enforces them at construction. Config does not import the codec
// at runtime, so a change to either set of constants alone would only surface
// as a startup failure on a value config had already accepted.
func TestCompressionConfig_ChunkBoundsMatchCodec(t *testing.T) {
	t.Parallel()
	if MinCompressionChunkSize != compression.MinChunkSize {
		t.Errorf("MinCompressionChunkSize = %d, codec MinChunkSize = %d",
			MinCompressionChunkSize, compression.MinChunkSize)
	}
	if MaxCompressionChunkSize != compression.MaxChunkSize {
		t.Errorf("MaxCompressionChunkSize = %d, codec MaxChunkSize = %d",
			MaxCompressionChunkSize, compression.MaxChunkSize)
	}
	if DefaultCompressionChunkSize != compression.DefaultChunkSize {
		t.Errorf("DefaultCompressionChunkSize = %d, codec DefaultChunkSize = %d",
			DefaultCompressionChunkSize, compression.DefaultChunkSize)
	}
}

// TestCompressionConfig_LevelsMatchEncoder pins the accepted names to the
// encoder's own set. The point of a named level is that it maps one-to-one
// onto a level zstd actually distinguishes, which holds only while these two
// lists agree.
func TestCompressionConfig_LevelsMatchEncoder(t *testing.T) {
	t.Parallel()
	encoderLevels := []zstd.EncoderLevel{
		zstd.SpeedFastest,
		zstd.SpeedDefault,
		zstd.SpeedBetterCompression,
		zstd.SpeedBestCompression,
	}
	if len(compressionLevels) != len(encoderLevels) {
		t.Fatalf("config accepts %d levels, encoder distinguishes %d",
			len(compressionLevels), len(encoderLevels))
	}
	for i, name := range compressionLevels {
		if name != encoderLevels[i].String() {
			t.Errorf("level %d: config has %q, encoder has %q", i, name, encoderLevels[i].String())
		}
		ok, parsed := zstd.EncoderLevelFromString(name)
		if !ok || parsed != encoderLevels[i] {
			t.Errorf("level %q: encoder parsed ok=%v as %v, want %v", name, ok, parsed, encoderLevels[i])
		}
	}
}

// TestCompressionConfig_DefaultLevelMatchesCodec verifies the default name
// resolves to the level the codec documents as its default.
func TestCompressionConfig_DefaultLevelMatchesCodec(t *testing.T) {
	t.Parallel()
	ok, parsed := zstd.EncoderLevelFromString(DefaultCompressionLevel)
	if !ok {
		t.Fatalf("default level %q is not an encoder level", DefaultCompressionLevel)
	}
	if want := zstd.EncoderLevelFromZstd(compression.DefaultLevel); parsed != want {
		t.Errorf("default level %q resolves to %v, codec DefaultLevel %d resolves to %v",
			DefaultCompressionLevel, parsed, compression.DefaultLevel, want)
	}
}
