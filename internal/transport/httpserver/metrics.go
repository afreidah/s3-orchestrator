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

// mountPprof wires the net/http/pprof handlers on mux. It must only be used on
// the dedicated metrics listener, never one that accepts user traffic.
func mountPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

// configureMetrics either registers /metrics on mux or returns a separate
// metrics listener, which the caller starts and shuts down. Returns nil when
// metrics are disabled or served inline. Pprof is mounted only when cfg.Pprof
// is set and the dedicated listener is in use (cfg.Listen set).
func configureMetrics(mux *http.ServeMux, cfg *config.MetricsConfig) *http.Server {
	if !cfg.Enabled {
		return nil
	}

	if cfg.Listen != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle(cfg.Path, promhttp.Handler())
		pprofMounted := false
		if cfg.Pprof {
			mountPprof(metricsMux)
			pprofMounted = true
		}
		slog.InfoContext(context.Background(), "metrics endpoint enabled on dedicated listener",
			logfmt.Component("httpserver"),
			"listen", cfg.Listen,
			"path", cfg.Path,
			"pprof", pprofMounted,
		)
		return &http.Server{
			Addr:              cfg.Listen,
			Handler:           metricsMux,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	if cfg.Pprof {
		slog.WarnContext(context.Background(), "telemetry.metrics.pprof=true ignored: pprof requires telemetry.metrics.listen (dedicated listener); not mounting on main S3 listener",
			logfmt.Component("httpserver"),
		)
	}
	mux.Handle(cfg.Path, promhttp.Handler())
	slog.InfoContext(context.Background(), "metrics endpoint enabled on main listener",
		logfmt.Component("httpserver"),
		"path", cfg.Path,
	)
	return nil
}
