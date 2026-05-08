// -------------------------------------------------------------------------------
// Auth Metrics
//
// Author: Alex Freidah
//
// Counters for authentication and streaming-payload validation outcomes.
// AuthStreamingRequestsTotal tracks the number of streaming-payload PUTs
// received by variant; AuthStreamingRejectionsTotal tracks the number of
// requests rejected mid-stream by reason. Together they let operators
// distinguish "we are seeing streaming traffic" from "streaming traffic
// is failing the integrity check" without log scraping.
// -------------------------------------------------------------------------------

package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// AuthStreamingRequestsTotal counts streaming-payload requests received,
// labeled by AWS streaming variant. Incremented at the moment the
// transport layer wraps the request body in a chunk-validating reader.
var AuthStreamingRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "s3o_auth_streaming_requests_total",
		Help: "Total streaming-payload SigV4 requests received, by AWS streaming variant.",
	},
	[]string{"variant"},
)

// AuthStreamingRejectionsTotal counts streaming-payload requests
// rejected due to a chunk- or trailer-validation failure, labeled by
// reason. Reasons map 1:1 to the auth.Err* sentinels so dashboards can
// distinguish a tampered body (chunk_signature_mismatch) from a
// malformed framing attempt (chunk_malformed).
var AuthStreamingRejectionsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "s3o_auth_streaming_rejections_total",
		Help: "Total streaming-payload requests rejected mid-stream, by reason.",
	},
	[]string{"reason"},
)
