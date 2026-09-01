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
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samber/do/v2"
	"golang.org/x/sync/errgroup"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// Deps holds the dependencies New requires.
type Deps struct {
	Cfg       *config.Config
	Mode      config.Mode
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

	// requireMetrics aborts startup when the metrics listener cannot bind,
	// rather than serving S3 traffic with no metrics.
	requireMetrics bool

	// Bound sockets, held between Listen and Run. Listening separately from
	// serving is what makes an address conflict a startup failure instead of a
	// goroutine that logs and exits after the process has already reported
	// itself ready.
	mainLn    net.Listener
	metricsLn net.Listener
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

	if deps.Mode.IsAPI() {
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
		main:           main,
		metrics:        metricsSrv,
		certReloader:   reloader,
		requireMetrics: deps.Cfg.Telemetry.Metrics.ListenerRequired(),
		log:            slog.Default().With(logfmt.Component("httpserver")),
	}, nil
}

// CertReloader returns the TLS cert reloader, or nil when TLS is not
// configured. The reload coordinator calls Reload on this to refresh
// certificates on SIGHUP.
func (s *Server) CertReloader() *httputil.CertReloader {
	return s.certReloader
}

// Listen binds every socket the server will serve on, without accepting
// anything yet. Call it before reporting the process ready: a bind that fails
// here is a startup error the caller can act on, where the same failure
// discovered inside Run is a goroutine exiting after readiness has already
// been announced and the orchestrator is taking traffic.
//
// A metrics bind failure aborts unless telemetry.metrics.require_listener is
// false, which is the dev and embedded case where the port may well be taken
// and best-effort metrics are fine.
func (s *Server) Listen(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.main.Addr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", s.main.Addr, err)
	}
	s.mainLn = ln

	if s.metrics == nil {
		return nil
	}

	metricsLn, err := lc.Listen(ctx, "tcp", s.metrics.Addr)
	if err != nil {
		if s.requireMetrics {
			_ = ln.Close()
			s.mainLn = nil
			return fmt.Errorf("bind metrics listener %s: %w", s.metrics.Addr, err)
		}
		// Opted out of requiring it: carry on without metrics, but say so at
		// WARN rather than leaving an operator to infer it from an empty graph.
		s.log.WarnContext(ctx, "metrics listener could not bind; continuing without it",
			"listen", s.metrics.Addr, "error", err,
			"detail", "set telemetry.metrics.require_listener to make this fail startup instead")
		s.metrics = nil
		return nil
	}
	s.metricsLn = metricsLn
	return nil
}

// Run serves on the sockets Listen bound and blocks until one of them stops.
// Whichever stops first, the other is shut down with it: an orchestrator
// serving S3 traffic with a dead metrics listener looks healthy while
// Prometheus receives nothing, which is the failure this reports rather than
// logs.
//
// A shutdown initiated through Shutdown closes both listeners and surfaces as
// http.ErrServerClosed on each, which is not an error and is not returned.
func (s *Server) Run(ctx context.Context) error {
	if s.mainLn == nil {
		if err := s.Listen(ctx); err != nil {
			return err
		}
	}

	// Whichever listener stops first stops the other, so Wait cannot block on a
	// survivor. Done here rather than by watching the errgroup's context: that
	// context is only cancelled once Wait returns, which is precisely the thing
	// waiting on it.
	var once sync.Once
	stopAll := func() {
		once.Do(func() {
			shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
			defer cancel()
			s.closeListeners(shutCtx)
		})
	}

	var g errgroup.Group

	if s.metricsLn != nil {
		s.log.InfoContext(ctx, "metrics endpoint enabled on separate listener", "listen", s.metrics.Addr)
		g.Go(func() error {
			defer stopAll()
			if err := s.metrics.Serve(s.metricsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("metrics listener: %w", err)
			}
			return nil
		})
	}

	g.Go(func() error {
		defer stopAll()
		var err error
		if s.main.TLSConfig != nil {
			err = s.main.ServeTLS(s.mainLn, "", "")
		} else {
			err = s.main.Serve(s.mainLn)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("main listener: %w", err)
		}
		return nil
	})

	return g.Wait()
}

// shutdownGrace bounds the teardown of the surviving listener when its sibling
// has already failed. Short: the process is on its way down either way, and the
// runtime's own shutdown sequence is what drains connections properly.
const shutdownGrace = 5 * time.Second

// closeListeners stops both HTTP servers, ignoring the errors: this runs on a
// path that is already failing, and the caller reports that failure.
func (s *Server) closeListeners(ctx context.Context) {
	if s.metrics != nil {
		_ = s.metrics.Shutdown(ctx)
	}
	_ = s.main.Shutdown(ctx)
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
