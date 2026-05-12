// -------------------------------------------------------------------------------
// HTTP Server - Assembly, Startup, and Shutdown
//
// Author: Alex Freidah
//
// Builds the daemon's *http.Server (and optional separate metrics listener),
// wiring health, metrics, admin, UI, and S3 routes together with TLS, mTLS,
// and the admission/rate-limit middleware stack. The runtime owns the
// lifecycle - this package only constructs the listener and exposes Start
// and Shutdown for the runtime to call.
// -------------------------------------------------------------------------------

// Package httpserver constructs the HTTP listener the daemon serves S3,
// admin, UI, health, and metrics traffic on. It owns route registration,
// middleware composition, TLS config (including the cert reloader for
// SIGHUP rotation), and the optional separate metrics listener.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// Daemon mode strings. registerS3Handler and registerUIHandler are only
// mounted in "api" and "all" modes; worker-only mode exposes neither.
const (
	ModeAPI = "api"
	ModeAll = "all"
)

// Deps groups every external the HTTP server needs at construction time.
// The struct keeps the New signature readable as the daemon grows new
// optional surfaces.
type Deps struct {
	Cfg       *config.Config
	Mode      string
	Injector  do.Injector
	Ready     *atomic.Bool
	DBBreaker func() *breaker.CircuitBreaker
}

// Server bundles the main HTTP listener with its optional separate metrics
// listener and the TLS cert reloader. Run starts both listeners; Shutdown
// closes them in the right order. The cert reloader is exposed so the
// reload coordinator can refresh certificates without reaching into the
// listener internals.
type Server struct {
	main         *http.Server
	metrics      *http.Server
	certReloader *httputil.CertReloader
	log          *slog.Logger
}

// New constructs the HTTP server with all routes mounted and TLS
// configured. It does not start any listener; call Run to do that.
func New(deps Deps) (*Server, error) {
	if deps.Cfg == nil {
		return nil, errors.New("httpserver: nil config")
	}
	if deps.Ready == nil {
		return nil, errors.New("httpserver: nil ready flag")
	}
	if deps.DBBreaker == nil {
		return nil, errors.New("httpserver: nil DBBreaker callback")
	}

	mux := http.NewServeMux()
	metricsSrv := configureMetrics(mux, &deps.Cfg.Telemetry.Metrics)
	registerHealthEndpoints(mux, HealthDeps{Ready: deps.Ready, DBBreaker: deps.DBBreaker})

	if err := registerAdminHandler(mux, deps.Injector, deps.Cfg); err != nil {
		return nil, err
	}

	if deps.Mode == ModeAPI || deps.Mode == ModeAll {
		if err := registerUIHandler(mux, deps.Injector, deps.Cfg); err != nil {
			return nil, err
		}
		if err := registerS3Handler(mux, deps.Injector, deps.Cfg); err != nil {
			return nil, err
		}
	}

	main := &http.Server{
		Addr:              deps.Cfg.Server.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: deps.Cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       deps.Cfg.Server.ReadTimeout,
		WriteTimeout:      deps.Cfg.Server.WriteTimeout,
		IdleTimeout:       deps.Cfg.Server.IdleTimeout,
	}

	tlsCfg, reloader, err := buildTLSConfig(&deps.Cfg.Server.TLS)
	if err != nil {
		return nil, fmt.Errorf("configure TLS: %w", err)
	}
	main.TLSConfig = tlsCfg

	return &Server{
		main:         main,
		metrics:      metricsSrv,
		certReloader: reloader,
		log:          slog.Default().With(logfmt.Component("httpserver")),
	}, nil
}

// CertReloader returns the TLS cert reloader, or nil when TLS is not
// configured. The reload coordinator calls Reload on this to refresh
// certificates on SIGHUP.
func (s *Server) CertReloader() *httputil.CertReloader {
	return s.certReloader
}

// Run starts the metrics listener (if separate) and the main listener.
// It blocks until either listener returns. The error channel returned by
// the main listener is forwarded so the runtime can distinguish a
// natural shutdown (http.ErrServerClosed) from a real failure.
func (s *Server) Run(ctx context.Context) error {
	if s.metrics != nil {
		go func() {
			s.log.InfoContext(ctx, "metrics endpoint enabled on separate listener",
				"listen", s.metrics.Addr)
			if err := s.metrics.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.log.ErrorContext(ctx, "metrics listener failed", "error", err)
			}
		}()
	}

	var err error
	if s.main.TLSConfig != nil {
		err = s.main.ListenAndServeTLS("", "")
	} else {
		err = s.main.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown drains the main listener (with the supplied context's
// timeout) and the metrics listener if present. Errors are logged but
// do not abort the shutdown sequence; the runtime owns final teardown
// ordering.
func (s *Server) Shutdown(ctx context.Context) {
	if err := s.main.Shutdown(ctx); err != nil {
		s.log.ErrorContext(ctx, "HTTP server shutdown error", "error", err)
	}
	if s.metrics != nil {
		if err := s.metrics.Shutdown(ctx); err != nil {
			s.log.ErrorContext(ctx, "metrics server shutdown error", "error", err)
		}
	}
}

// DrainTimeout is the default Shutdown deadline used by the runtime when
// the caller does not supply one. Exported so tests can match the
// production value without coupling to a magic number.
const DrainTimeout = 30 * time.Second
