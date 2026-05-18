// -------------------------------------------------------------------------------
// HTTP Server - Prometheus Metrics Endpoint
//
// Author: Alex Freidah
//
// Wires /metrics in one of two shapes: inline on the main mux, or as a
// separate http.Server bound to a private listener address. Operators
// typically use the separate listener so scrapes do not contend with S3
// traffic, but the inline form is supported for single-port deployments.
// -------------------------------------------------------------------------------

package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
)

// mountPprof wires the standard net/http/pprof handlers on the supplied
// mux. The pprof endpoints expose deep runtime state (de-anonymized
// stack frames, command-line flags, on-demand CPU profiles that double
// as DoS amplifiers) so they MUST NOT be mounted on any listener that
// accepts user traffic. configureMetrics only calls this helper when a
// dedicated metrics listener is configured (typically bound to an
// internal-only interface); operators who run inline metrics on the
// main S3 listener intentionally do not get pprof.
func mountPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

// configureMetrics either registers /metrics on mux or returns a separate
// metrics listener for the runtime to start. Returns nil when metrics are
// disabled. The separate listener is the caller's responsibility to start
// and shut down; this function only constructs it.
//
// When the dedicated listener form is used, pprof is also mounted on
// that listener so operators can profile a running process without
// reproducing the workload locally. Inline-metrics deployments do not
// get pprof - registering /debug/pprof/* on the main mux would expose
// runtime internals on the same listener that serves the S3 API.
func configureMetrics(mux *http.ServeMux, cfg *config.MetricsConfig) *http.Server {
	if !cfg.Enabled {
		return nil
	}

	if cfg.Listen != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle(cfg.Path, promhttp.Handler())
		mountPprof(metricsMux)
		slog.InfoContext(context.Background(), "metrics + pprof endpoints enabled on dedicated listener",
			logfmt.Component("httpserver"),
			"listen", cfg.Listen,
			"path", cfg.Path,
		)
		return &http.Server{
			Addr:              cfg.Listen,
			Handler:           metricsMux,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	mux.Handle(cfg.Path, promhttp.Handler())
	slog.InfoContext(context.Background(), "metrics endpoint enabled on main listener (pprof intentionally not mounted - set telemetry.metrics.listen to enable pprof on a dedicated internal listener)",
		logfmt.Component("httpserver"),
		"path", cfg.Path,
	)
	return nil
}
