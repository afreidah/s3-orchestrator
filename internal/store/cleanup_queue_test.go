// -------------------------------------------------------------------------------
// Cleanup Queue Tests
//
// Author: Alex Freidah
//
// Tests for the durationToInterval helper used by cleanup queue retry logic.
// The store methods themselves are thin DB wrappers tested via integration tests.
// -------------------------------------------------------------------------------

package store

import (
	"testing"
	"time"
)

func TestDurationToInterval(t *testing.T) {
	tests := []struct {
		name     string
		d        time.Duration
		wantUS   int64
	}{
		{"one minute", time.Minute, 60_000_000},
		{"one hour", time.Hour, 3_600_000_000},
		{"24 hours", 24 * time.Hour, 86_400_000_000},
		{"zero", 0, 0},
		{"sub-millisecond", 500 * time.Microsecond, 500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iv := durationToInterval(tt.d)
			if !iv.Valid {
				t.Error("interval should be valid")
			}
			if iv.Microseconds != tt.wantUS {
				t.Errorf("Microseconds = %d, want %d", iv.Microseconds, tt.wantUS)
			}
		})
	}
}
