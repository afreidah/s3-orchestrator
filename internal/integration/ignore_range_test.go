// -------------------------------------------------------------------------------
// Integration Tests - Backends That Ignore Range
//
// Author: Alex Freidah
//
// Some S3 gateways answer a ranged GET with 200 and the whole object. A proxy
// in front of MinIO plays that backend here, and every stored form is read
// whole and by range through it, byte for byte against what was written.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// noRangeBackend names the one backend in each fleet below, so every read is
// served by the backend that ignores Range.
const noRangeBackend = "h-no-range"

// TestIgnoredRange_EveryStoredFormReadsCorrectly reads plaintext, encrypted,
// and compressed-and-encrypted objects whole and by range from a backend that
// ignores Range, and requires the backend layer to have cut every range.
func TestIgnoredRange_EveryStoredFormReadsCorrectly(t *testing.T) {
	cases := []struct {
		name string
		spec harnessSpec
	}{
		{name: "plaintext"},
		{name: "encrypted", spec: harnessSpec{Encrypt: true}},
		{name: "compressed and encrypted", spec: harnessSpec{Compression: compressionOn(), Encrypt: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			spec.Backends = []harnessBackend{{Name: noRangeBackend, Quota: 64 << 20, IgnoreRange: true}}
			h := newHarness(t, spec)

			key := uniqueKey(t, "no-range")
			body := partlyCompressible(compTestChunk * 4)
			h.put(key, body)

			if got := h.get(key); !bytes.Equal(got, body) {
				t.Fatalf("whole-object read returned %d bytes, want %d", len(got), len(body))
			}
			ignored := telemetry.BackendRangeIgnoredTotal.WithLabelValues(noRangeBackend)
			before := testutil.ToFloat64(ignored)
			assertRanges(t, h, key, body)
			if after := testutil.ToFloat64(ignored); after <= before {
				t.Errorf("no ranged read reached the backend ignoring Range: counter %v -> %v", before, after)
			}
		})
	}
}
