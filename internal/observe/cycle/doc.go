// Package cycle is the one reporter of a background pass. Run puts a pass
// under its workload, request ID and span, counts it in the cycle metrics,
// writes one completion audit entry when the pass had work, and refreshes the
// quota gauges when the pass changed what backends hold. No pass reports its
// own cycles.
package cycle
