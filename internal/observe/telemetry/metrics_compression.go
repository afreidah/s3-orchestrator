// -------------------------------------------------------------------------------
// Compression Metrics
//
// Author: Alex Freidah
//
// What compression is worth and what it costs. The write side reports bytes in
// against bytes stored, so both the ratio a fleet achieves and the bytes it
// saves come from the same pair rather than from a figure that has to be
// maintained. The read side reports bytes fetched against bytes served, which
// is the number that proves ranged reads are still ranged.
//
// Also the counters for the bulk passes an operator runs to bring an existing
// fleet under compression or take it back out.
// -------------------------------------------------------------------------------

package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Write-side volume. The two counters are a pair on purpose: the ratio a fleet
// achieves is stored/logical over any window, and the bytes it saved is
// logical - stored, so neither has to be tracked separately or kept in step.
//
// Only objects actually stored encoded are counted. Folding in the ones that
// were skipped would report a ratio no encoder produced.
// CompressionLogicalBytesTotal counts the bytes clients wrote for objects that
// were then stored encoded; the stored counter beside it is what they occupy.
var (
	CompressionLogicalBytesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_compression_logical_bytes_total",
			Help: "Logical bytes of objects stored compressed, before encoding",
		},
	)

	// CompressionStoredBytesTotal counts what those objects actually occupy.
	CompressionStoredBytesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_compression_stored_bytes_total",
			Help: "Bytes those objects occupy on backends after encoding",
		},
	)

	// CompressionRatio observes the ratio one object reached, which the two
	// counters above cannot show: a fleet averaging 0.4 behaves differently
	// depending on whether that is every object or half of them at 0.05.
	CompressionRatio = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "s3o_compression_ratio",
			Help:    "Encoded size as a fraction of logical size, per stored object",
			Buckets: prometheus.LinearBuckets(0.05, 0.05, 20),
		},
	)
)

// Read-side volume, which together give read amplification: fetched/served. For
// a ranged read of a compressed object that figure is the frames the range
// touched over the bytes the client asked for, so it is bounded by the chunk
// size and rises only if something starts fetching more than it needs. A
// regression to whole-object decode shows here and nowhere else except the
// backend bill.
var (
	CompressionFetchedBytesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_compression_fetched_bytes_total",
			Help: "Stored bytes fetched from backends while serving compressed objects",
		},
	)

	// CompressionServedBytesTotal counts logical bytes handed to clients from
	// those reads.
	CompressionServedBytesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_compression_served_bytes_total",
			Help: "Logical bytes served to clients from compressed objects",
		},
	)
)

// RecordCompressed reports one object stored encoded. The three instruments
// move together or the ratio they describe is not the ratio that happened, so
// no call site updates them individually.
func RecordCompressed(logicalSize, storedSize int64) {
	if logicalSize <= 0 {
		return
	}
	CompressionLogicalBytesTotal.Add(float64(logicalSize))
	CompressionStoredBytesTotal.Add(float64(storedSize))
	CompressionRatio.Observe(float64(storedSize) / float64(logicalSize))
}

// CompressionSkippedTotal counts objects a write declined to encode. The reason
// separates the two floors: size is answered before any encoding, ratio only
// after one that turned out not to be worth keeping.
var CompressionSkippedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "s3o_compression_skipped_total",
		Help: "Objects stored verbatim despite compression being enabled, by reason",
	},
	[]string{"reason"},
)

// CompressionErrorsTotal counts codec failures by which direction failed.
// A decode failure is the serious one: it means bytes already stored cannot be
// read back, whereas an encode failure only fails the write that caused it.
var CompressionErrorsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "s3o_compression_errors_total",
		Help: "Codec failures, by operation (encode, decode)",
	},
	[]string{"operation"},
)

// Reasons an object was stored verbatim, and the codec operations that can
// fail. Named so a call site cannot invent a label value the dashboard does not
// query for.
const (
	CompressionSkipMinSize  = "min_size"
	CompressionSkipMinRatio = "min_ratio"
	CompressionOpEncode     = "encode"
	CompressionOpDecode     = "decode"
)

// The status label carries success, skipped or error. Skipped is its own value
// rather than folded into success because it means the pass deliberately left
// an object alone - too small, or too incompressible to be worth encoding - and
// an operator reading a run wants that separate from work that was done.
var (
	CompressExistingObjectsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_compress_existing_objects_total",
			Help: "Total objects processed during compress-existing operation",
		},
		[]string{"status"},
	)

	// DecompressExistingObjectsTotal counts objects processed during decompress-existing.
	DecompressExistingObjectsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_decompress_existing_objects_total",
			Help: "Total objects processed during decompress-existing operation",
		},
		[]string{"status"},
	)
)
