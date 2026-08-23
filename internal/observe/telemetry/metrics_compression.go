// -------------------------------------------------------------------------------
// Compression Metrics
//
// Author: Alex Freidah
//
// Counters for the bulk compression passes an operator runs to bring an
// existing fleet under compression, or to take it back out. Per-request
// compression is covered by the object metrics; these describe maintenance
// work, which is why they count objects rather than bytes.
// -------------------------------------------------------------------------------

package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The status label carries success, skipped or error. Skipped is its own value
// rather than folded into success because it means the pass deliberately left
// an object alone - too small, or too incompressible to be worth encoding - and
// an operator reading a run wants that separate from work that was done.
var (
	// CompressExistingObjectsTotal counts objects processed during compress-existing.
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
