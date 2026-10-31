// -------------------------------------------------------------------------------
// Metrics  -  CORS
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface covering browser
// preflight outcomes. A preflight is answered before the request reaches the
// S3 handler, so it appears in none of the request metrics; this counter is
// the only place a refused browser upload is visible server-side.
// -------------------------------------------------------------------------------

package telemetry

// CORSPreflightTotal counts preflight requests by outcome, labelled
// allowed or rejected. A climbing rejected count with no allowed count is
// the signature of a bucket whose rules do not cover the origin the
// application is served from. Read by the CORS panel on the dashboard.
var CORSPreflightTotal = counterVec("s3o_cors_preflight_total", "Browser CORS preflight requests by outcome", "result")
