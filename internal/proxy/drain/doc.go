// Package drain owns the operator side of the backend drain and remove
// lifecycle: starting, cancelling, and reporting a drain through its record,
// and exposing IsDraining for the proxy core's eligibility filters. The
// migration itself is the drainer worker's.
package drain
