// -------------------------------------------------------------------------------
// Compression Metrics Tests
//
// Author: Alex Freidah
//
// The compression metrics exist to answer two questions an operator cannot
// answer any other way: what the feature is saving, and whether ranged reads are
// still ranged. Both are ratios between two counters, so a counter that moves on
// the wrong path or not at all reports a ratio that is quietly wrong rather than
// obviously missing. These tests pin which path moves which counter.
//
// They do not run in parallel: the instruments are process-global, so a
// concurrent test writing the same counter would make the deltas meaningless.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"io"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// TestCompressionMetrics_WriteRecordsBothSides checks a compressed write moves
// the logical and stored counters together. Their ratio is what the dashboard
// reports as the fleet's compression ratio, and their difference is the bytes
// saved, so one moving without the other is a wrong answer rather than a gap.
func TestCompressionMetrics_WriteRecordsBothSides(t *testing.T) {
	codec := newPutCodec(t)
	src := compressibleBody(putCompressChunk * 3)

	logicalBefore := testutil.ToFloat64(telemetry.CompressionLogicalBytesTotal)
	storedBefore := testutil.ToFloat64(telemetry.CompressionStoredBytesTotal)

	res := putThroughFleet(t, &fleetOpts{Codec: codec, Compression: compressionOn(0)}, "key", src)

	logicalDelta := testutil.ToFloat64(telemetry.CompressionLogicalBytesTotal) - logicalBefore
	storedDelta := testutil.ToFloat64(telemetry.CompressionStoredBytesTotal) - storedBefore
	if logicalDelta != float64(len(src)) {
		t.Errorf("logical bytes delta = %v, want %d", logicalDelta, len(src))
	}
	if storedDelta != float64(len(res.stored)) {
		t.Errorf("stored bytes delta = %v, want %d", storedDelta, len(res.stored))
	}
	if storedDelta >= logicalDelta {
		t.Errorf("stored %v >= logical %v; the pair cannot report a saving", storedDelta, logicalDelta)
	}
}

// TestCompressionMetrics_SkipReasons checks the two floors are distinguishable.
// An operator reading a fleet that compresses nothing needs to know whether the
// objects are too small or too incompressible, because the fixes differ.
func TestCompressionMetrics_SkipReasons(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		body   []byte
		minSze int64
	}{
		{"below the size floor", telemetry.CompressionSkipMinSize, compressibleBody(512), 4096},
		{"misses the ratio", telemetry.CompressionSkipMinRatio, incompressibleBody(t, putCompressChunk*2), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := testutil.ToFloat64(telemetry.CompressionSkippedTotal.WithLabelValues(tt.reason))

			putThroughFleet(t, &fleetOpts{
				Codec:       newPutCodec(t),
				Compression: compressionOn(tt.minSze),
			}, "key", tt.body)

			if got := testutil.ToFloat64(telemetry.CompressionSkippedTotal.WithLabelValues(tt.reason)); got != before+1 {
				t.Errorf("%s counter = %v, want %v", tt.reason, got, before+1)
			}
		})
	}
}

// TestCompressionMetrics_ReadAmplification checks a ranged read of a compressed
// object reports both sides of the amplification ratio, and that a small range
// fetches on the order of a frame rather than the whole object. That ratio is
// the only signal that would catch a regression to whole-object decode.
func TestCompressionMetrics_ReadAmplification(t *testing.T) {
	codec := newPutCodec(t)
	// Incompressible on purpose: the stored object has to be much larger than
	// the reader's tail prefetch, or one fetch covers the whole thing and the
	// assertion below proves nothing. A compressible fixture this size encodes
	// to under a kilobyte.
	plain := incompressibleBody(t, putCompressChunk*8)

	var stored bytes.Buffer
	if _, err := codec.Compress(&stored, bytes.NewReader(plain)); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	be := backendtest.NewInMemory()
	if _, err := be.PutObject(context.Background(), "bucket/obj", bytes.NewReader(stored.Bytes()),
		int64(stored.Len()), "application/octet-stream", nil); err != nil {
		t.Fatalf("seed backend: %v", err)
	}

	loc := core.ObjectLocation{
		ObjectKey:                "bucket/obj",
		BackendName:              "b1",
		SizeBytes:                int64(stored.Len()),
		CompressionAlgorithm:     "zstd",
		CompressionFormatVersion: 1,
		LogicalSize:              int64(len(plain)),
	}
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetAllObjectLocations(gomock.Any(), gomock.Any()).
		Return([]core.ObjectLocation{loc}, nil).AnyTimes()
	storetest.Permissive(store)

	f := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, &fleetOpts{
		Order: []string{"b1"}, Codec: codec,
	})

	fetchedBefore := testutil.ToFloat64(telemetry.CompressionFetchedBytesTotal)
	servedBefore := testutil.ToFloat64(telemetry.CompressionServedBytesTotal)

	const wantServed = 100
	res, err := f.GetObject(context.Background(), "bucket/obj", "bytes=0-99")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatalf("drain body: %v", err)
	}
	_ = res.Body.Close()

	served := testutil.ToFloat64(telemetry.CompressionServedBytesTotal) - servedBefore
	fetched := testutil.ToFloat64(telemetry.CompressionFetchedBytesTotal) - fetchedBefore
	if served != wantServed {
		t.Errorf("served bytes delta = %v, want %d", served, wantServed)
	}
	if fetched <= 0 {
		t.Fatal("no fetched bytes recorded; read amplification would divide by nothing")
	}
	// The point of the chunked format: a 100 byte range costs the frames it
	// touches, not the object. Anything approaching the stored size means the
	// read stopped being ranged.
	if fetched >= float64(stored.Len()) {
		t.Errorf("fetched %v of a %d byte object for a %d byte range; the read was not ranged",
			fetched, stored.Len(), wantServed)
	}
}
