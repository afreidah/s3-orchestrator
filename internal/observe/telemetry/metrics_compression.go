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

import "github.com/prometheus/client_golang/prometheus"

// Write-side volume, counting only objects stored encoded. The fleet ratio is
// stored/logical over any window, and the bytes saved are logical - stored.
// CompressionLogicalBytesTotal counts the bytes clients wrote for those objects.
var (
	CompressionLogicalBytesTotal = counter("s3o_compression_logical_bytes_total",
		"Logical bytes of objects stored compressed, before encoding")

	// CompressionStoredBytesTotal counts what those objects actually occupy.
	CompressionStoredBytesTotal = counter("s3o_compression_stored_bytes_total",
		"Bytes those objects occupy on backends after encoding")

	// CompressionRatio observes the ratio one object reached, which the two
	// counters above cannot show: a fleet averaging 0.4 behaves differently
	// depending on whether that is every object or half of them at 0.05.
	CompressionRatio = histogram("s3o_compression_ratio",
		"Encoded size as a fraction of logical size, per stored object", prometheus.LinearBuckets(0.05, 0.05, 20))
)

// Read-side volume, which together give read amplification: fetched/served.
// It is bounded by the chunk size, so a rise means reads are fetching more
// frames than a range needs. CompressionFetchedBytesTotal counts stored bytes
// fetched from backends.
var (
	CompressionFetchedBytesTotal = counter("s3o_compression_fetched_bytes_total",
		"Stored bytes fetched from backends while serving compressed objects")

	// CompressionServedBytesTotal counts logical bytes handed to clients from
	// those reads.
	CompressionServedBytesTotal = counter("s3o_compression_served_bytes_total",
		"Logical bytes served to clients from compressed objects")
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
var CompressionSkippedTotal = counterVec("s3o_compression_skipped_total",
	"Objects stored verbatim despite compression being enabled, by reason", "reason")

// CompressionErrorsTotal counts codec failures by which direction failed.
// A decode failure is the serious one: it means bytes already stored cannot be
// read back, whereas an encode failure only fails the write that caused it.
var CompressionErrorsTotal = counterVec("s3o_compression_errors_total",
	"Codec failures, by operation (encode, decode)", "operation")

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
	CompressExistingObjectsTotal = counterVec("s3o_compress_existing_objects_total",
		"Total objects processed during compress-existing operation", "status")

	// DecompressExistingObjectsTotal counts objects processed during decompress-existing.
	DecompressExistingObjectsTotal = counterVec("s3o_decompress_existing_objects_total",
		"Total objects processed during decompress-existing operation", "status")
)
