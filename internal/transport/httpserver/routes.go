// -------------------------------------------------------------------------------
// HTTP Server - Route Registration
//
// Author: Alex Freidah
//
// Mounts admin, UI, and S3 handlers on the main mux with the configured
// middleware stack: rate limiting (optional), admission control (split or
// single-channel), request shedding, and browser CORS. Route registration is
// gated by daemon mode so the worker-only mode does not expose S3 or UI
// surfaces.
// -------------------------------------------------------------------------------

package httpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/di"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/cors"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/transport/ui"
)

// registerAdminHandler mounts the admin API at /admin/. It is always mounted;
// access is decided by the grants each caller's credential holds.
func registerAdminHandler(mux *http.ServeMux, inj do.Injector, _ *config.Config) error {
	adminHandler, err := do.Invoke[*admin.Handler](inj)
	if err != nil {
		return fmt.Errorf("initialize admin handler: %w", err)
	}
	adminMux := http.NewServeMux()
	adminHandler.Register(adminMux)
	var adminHTTP http.Handler = adminMux
	rlRes := di.Optional[*s3api.RateLimiter](inj)
	if rlRes.Failed() {
		slog.WarnContext(context.Background(),
			"rate limiter resolution failed; admin API will run without rate limiting",
			logfmt.Component("httpserver"),
			"error", rlRes.Err)
	}
	if rl := rlRes.Value; rl != nil {
		adminHTTP = rl.Middleware(adminHTTP)
	}
	// Wraps the rate limiter too, so a panic anywhere in the admin chain
	// returns a JSON 500 with a request id instead of a reset connection.
	adminHTTP = httputil.PanicRecover("admin", adminPanicWriter)(adminHTTP)
	mux.Handle("/admin/", adminHTTP)
	slog.InfoContext(context.Background(), "admin API enabled",
		logfmt.Component("httpserver"),
		"path", "/admin/api/",
	)
	return nil
}

// registerUIHandler mounts the optional web UI dashboard. Panic recovery is
// not applied here: UI routes register directly on the shared mux, so they
// cannot be wrapped as one handler.
func registerUIHandler(mux *http.ServeMux, inj do.Injector, cfg *config.Config) error {
	if !cfg.UI.Enabled {
		return nil
	}
	h, err := do.Invoke[*ui.Handler](inj)
	if err != nil {
		return fmt.Errorf("initialize UI handler: %w", err)
	}
	h.Register(mux, cfg.UI.Path)
	slog.InfoContext(context.Background(), "web UI enabled",
		logfmt.Component("httpserver"),
		"path", cfg.UI.Path,
	)
	return nil
}

// registerS3Handler mounts the S3 proxy on / with optional rate limiting
// and admission control.
//
// Admission model (see internal/di/backend.go admissionSemFor):
//
//   - Split mode (MaxConcurrentReads and MaxConcurrentWrites): reads get a
//     fresh semaphore created here, local to the HTTP read path. Writes reuse
//     the runtime's AdmissionSem(), which is also the budget every background
//     worker acquires from, so HTTP writes share their ceiling with worker
//     activity and operators should size it for both.
//   - Merged mode (MaxConcurrentRequests): AdmissionSem() is one global pool
//     that HTTP reads, HTTP writes and workers all contend for.
//   - Neither set: no admission middleware is installed.
//
// Either form respects LoadShedThreshold and AdmissionWait when set.
//
// CORS sits inside both the rate limiter and admission control, so
// unauthenticated preflights are bounded by them too.
func registerS3Handler(mux *http.ServeMux, inj do.Injector, cfg *config.Config) error {
	rt, err := do.Invoke[*infra.BackendRuntime](inj)
	if err != nil {
		return fmt.Errorf("initialize backend runtime: %w", err)
	}
	s3Server, err := do.Invoke[*s3api.Server](inj)
	if err != nil {
		return fmt.Errorf("initialize S3 server: %w", err)
	}
	corsPolicy, err := do.Invoke[*cors.Policy](inj)
	if err != nil {
		return fmt.Errorf("initialize CORS policy: %w", err)
	}

	s3Handler := corsPolicy.Middleware(s3Server)
	rlRes := di.Optional[*s3api.RateLimiter](inj)
	if rlRes.Failed() {
		slog.WarnContext(context.Background(),
			"rate limiter resolution failed; S3 surface will run without rate limiting",
			logfmt.Component("httpserver"),
			"error", rlRes.Err)
	}
	if rl := rlRes.Value; rl != nil {
		s3Handler = rl.Middleware(s3Handler)
	}

	limits := s3api.AdmissionLimits{
		ShedThreshold: cfg.Server.LoadShedThreshold,
		Wait:          cfg.Server.AdmissionWait,
	}

	var ac *s3api.AdmissionController
	switch {
	case cfg.Server.MaxConcurrentReads > 0 && cfg.Server.MaxConcurrentWrites > 0:
		// Split-pool: dedicate a fresh read sem (HTTP-only) and reuse
		// the runtime's sem as the write+workers pool. See the func
		// doc above for the full model.
		readSem := make(chan struct{}, cfg.Server.MaxConcurrentReads)
		ac = s3api.NewSplitAdmissionControllerFromSem(readSem, rt.AdmissionSem(), limits)
	case cfg.Server.MaxConcurrentRequests > 0:
		// Merged-pool: every request and every worker shares the
		// runtime's sem.
		ac = s3api.NewAdmissionControllerFromSem(rt.AdmissionSem(), limits)
	}
	if ac != nil {
		s3Handler = ac.Middleware(s3Handler)
	}

	// Outermost, so a panic anywhere in the chain becomes an S3-XML 500
	// with a request id rather than a TCP RST.
	s3Handler = httputil.PanicRecover("s3", s3api.WriteS3Error)(s3Handler)

	mux.Handle("/", s3Handler)
	return nil
}

// -------------------------------------------------------------------------
// PANIC-RECOVERY WRITERS
// -------------------------------------------------------------------------

// adminPanicWriter writes the admin surface's panic response as a JSON error.
func adminPanicWriter(w http.ResponseWriter, status int, _ string, message string) {
	httputil.WriteJSONError(w, status, message)
}
