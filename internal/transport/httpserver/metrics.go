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
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// configureMetrics either registers /metrics on mux or returns a separate
// metrics listener for the runtime to start. Returns nil when metrics are
// disabled. The separate listener is the caller's responsibility to start
// and shut down; this function only constructs it.
func configureMetrics(mux *http.ServeMux, cfg *config.MetricsConfig) *http.Server {
	if !cfg.Enabled {
		return nil
	}

	if cfg.Listen != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle(cfg.Path, promhttp.Handler())
		return &http.Server{
			Addr:              cfg.Listen,
			Handler:           metricsMux,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	mux.Handle(cfg.Path, promhttp.Handler())
	slog.InfoContext(context.Background(), "metrics endpoint enabled", "path", cfg.Path)
	return nil
}
