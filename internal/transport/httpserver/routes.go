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

// registerAdminHandler mounts the admin API at /admin/ when the admin key is
// configured. Returns nil silently when the admin surface is disabled.
func registerAdminHandler(mux *http.ServeMux, inj do.Injector, cfg *config.Config) error {
	if cfg.UI.AdminKey == "" {
		return nil
	}
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
	// Panic-recovery middleware wraps the rate-limited handler so a
	// panic anywhere in the admin chain (handler or rate limiter)
	// produces a JSON 500 with a request id instead of a TCP RST
	//.
	adminHTTP = httputil.PanicRecover("admin", adminPanicWriter)(adminHTTP)
	mux.Handle("/admin/", adminHTTP)
	slog.InfoContext(context.Background(), "admin API enabled",
		logfmt.Component("httpserver"),
		"path", "/admin/api/",
	)
	return nil
}

// registerUIHandler mounts the optional web UI dashboard.
//
// Panic recovery is intentionally NOT applied to the UI surface.
// UI routes register themselves on the same mux as the S3 catch-all,
// so wrapping requires re-architecting the UI handler to expose its
// sub-routes for individual wrapping. UI is
// also the lowest panic-risk surface (mostly static reads from cached
// data, no streaming bodies); the bulk of the recovery value comes
// from S3 (large blast radius, complex paths) and admin (state-
// changing, attack surface).
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
// CORS wraps the S3 handler directly, inside both the rate limiter and
// admission control. A preflight carries no credentials, so answering it
// outside those two would leave the one request on this surface that anybody
// can send bounded by nothing.
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

	// Panic-recovery middleware wraps the entire S3 stack (admission
	// + rate-limit + handler) so a panic anywhere in the chain
	// produces an S3-XML 500 with a request id rather than a TCP RST.
	// Outermost wrap so it catches panics that escape the
	// inner middlewares too.
	s3Handler = httputil.PanicRecover("s3", s3api.WriteS3Error)(s3Handler)

	mux.Handle("/", s3Handler)
	return nil
}

// -------------------------------------------------------------------------
// PANIC-RECOVERY WRITERS
// -------------------------------------------------------------------------

// adminPanicWriter emits the admin surface's 500 response. Admin
// clients are HTTP/JSON, so the panic-recovery message goes back as
// a JSON {"error": "..."} body matching the rest of the admin error
// shape.
func adminPanicWriter(w http.ResponseWriter, status int, _ string, message string) {
	httputil.WriteJSONError(w, status, message)
}
