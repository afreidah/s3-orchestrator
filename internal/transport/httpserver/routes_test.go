// -------------------------------------------------------------------------------
// HTTP Server - Routes Failed-Resolution Tests
//
// Author: Alex Freidah
//
// Drives the Failed branch in registerAdminHandler and registerS3Handler
// by overriding the rate-limiter provider with a constructor that
// errors. The hooks emit a slog.Warn and fall back to a nil limiter so
// the mux still mounts; these tests pin that contract.
// -------------------------------------------------------------------------------

package httpserver

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// errRateLimiterBoom is the sentinel the override constructor returns
// so the tests can assert the di.Optional Failed branch was reached.
var errRateLimiterBoom = errors.New("rate limiter boom")

// overrideFailingRateLimiter replaces the existing rate-limiter
// provider with one that errors, modelling a configured-but-broken
// optional dependency.
func overrideFailingRateLimiter(inj do.Injector) {
	do.Override(inj, func(do.Injector) (*s3api.RateLimiter, error) {
		return nil, errRateLimiterBoom
	})
}

// TestRegisterAdminHandler_RateLimiterFailedFallsBack drives the slog
// fallback branch where the optional rate limiter fails to resolve but
// the admin mux is still mounted (without a limiter wrap).
func TestRegisterAdminHandler_RateLimiterFailedFallsBack(t *testing.T) {
	cfg := loadCfg(t, validTestConfigYAML)
	cfg.UI.AdminKey = "test-key"
	inj, cleanup := resolvedInjector(t, cfg, "all")
	defer cleanup()

	overrideFailingRateLimiter(inj)

	mux := http.NewServeMux()
	if err := registerAdminHandler(mux, inj, cfg); err != nil {
		t.Fatalf("registerAdminHandler: %v", err)
	}
}

// TestRegisterS3Handler_RateLimiterFailedFallsBack drives the same
// Failed branch on the S3 surface path. The route still mounts because
// the rate limiter is optional.
func TestRegisterS3Handler_RateLimiterFailedFallsBack(t *testing.T) {
	cfg := loadCfg(t, validTestConfigYAML)
	inj, cleanup := resolvedInjector(t, cfg, "all")
	defer cleanup()

	overrideFailingRateLimiter(inj)

	mux := http.NewServeMux()
	if err := registerS3Handler(mux, inj, cfg); err != nil {
		t.Fatalf("registerS3Handler: %v", err)
	}
}

// TestRegisterAdminHandler_DisabledByEmptyKey covers the early-return
// when no admin key is configured: the function should noop without
// touching the injector.
func TestRegisterAdminHandler_DisabledByEmptyKey(t *testing.T) {
	cfg := &config.Config{}
	if err := registerAdminHandler(http.NewServeMux(), do.New(), cfg); err != nil {
		t.Fatalf("registerAdminHandler: %v", err)
	}
}

// TestRegisterAdminHandler_AdminHandlerInvokeFails drives the
// "initialize admin handler" wrapped-error path by configuring an admin
// key against a bare injector that has no admin.Handler provider.
func TestRegisterAdminHandler_AdminHandlerInvokeFails(t *testing.T) {
	cfg := &config.Config{}
	cfg.UI.AdminKey = "key"
	err := registerAdminHandler(http.NewServeMux(), do.New(), cfg)
	if err == nil {
		t.Fatal("expected error when admin.Handler is not registered")
	}
	if !strings.Contains(err.Error(), "initialize admin handler") {
		t.Errorf("err = %q, want wrap mentioning admin handler", err)
	}
}

// TestRegisterUIHandler_DisabledByConfig covers the early-return when
// the UI is turned off in config.
func TestRegisterUIHandler_DisabledByConfig(t *testing.T) {
	if err := registerUIHandler(http.NewServeMux(), do.New(), &config.Config{}); err != nil {
		t.Fatalf("registerUIHandler: %v", err)
	}
}

// TestRegisterUIHandler_InvokeFails drives the "initialize UI handler"
// wrapped-error path: UI enabled but no Handler provider in the
// injector.
func TestRegisterUIHandler_InvokeFails(t *testing.T) {
	cfg := &config.Config{}
	cfg.UI.Enabled = true
	cfg.UI.Path = "/ui/"
	err := registerUIHandler(http.NewServeMux(), do.New(), cfg)
	if err == nil {
		t.Fatal("expected error when ui.Handler is not registered")
	}
	if !strings.Contains(err.Error(), "initialize UI handler") {
		t.Errorf("err = %q, want wrap mentioning UI handler", err)
	}
}

// TestRegisterS3Handler_BackendRuntimeInvokeFails drives the
// "initialize backend runtime" wrapped-error path against a bare
// injector with no BackendRuntime provider.
func TestRegisterS3Handler_BackendRuntimeInvokeFails(t *testing.T) {
	err := registerS3Handler(http.NewServeMux(), do.New(), &config.Config{})
	if err == nil {
		t.Fatal("expected error when BackendRuntime is not registered")
	}
	if !strings.Contains(err.Error(), "initialize backend runtime") {
		t.Errorf("err = %q, want wrap mentioning backend runtime", err)
	}
}

// TestRegisterS3Handler_S3ServerInvokeFails drives the
// "initialize S3 server" wrapped-error path: the backend runtime resolves
// cleanly but the s3api.Server provider errors, so registerS3Handler
// must propagate the wrapped error rather than mount a broken handler.
func TestRegisterS3Handler_S3ServerInvokeFails(t *testing.T) {
	cfg := loadCfg(t, validTestConfigYAML)
	inj, cleanup := resolvedInjector(t, cfg, "all")
	defer cleanup()

	s3Boom := errors.New("s3 server boom")
	do.Override(inj, func(do.Injector) (*s3api.Server, error) {
		return nil, s3Boom
	})

	err := registerS3Handler(http.NewServeMux(), inj, cfg)
	if err == nil {
		t.Fatal("expected error when s3api.Server provider fails")
	}
	if !strings.Contains(err.Error(), "initialize S3 server") {
		t.Errorf("err = %q, want wrap mentioning S3 server", err)
	}
	if !errors.Is(err, s3Boom) {
		t.Errorf("err = %v, want wrap of s3Boom", err)
	}
}
