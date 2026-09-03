// -------------------------------------------------------------------------------
// Backend Transport Tests
//
// Author: Alex Freidah
//
// The per-backend HTTP transport. What matters here is that a config block
// reaches the transport at all - the pool sizes and the HTTP/2 opt-out are the
// only way out of a throughput collapse against a backend the orchestrator
// cannot fix from its side.
// -------------------------------------------------------------------------------

package backend

import (
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// defaultedHTTPConfig returns a config block as the config layer hands it over:
// validated, with every unset field already filled in.
func defaultedHTTPConfig(t *testing.T, cfg config.BackendHTTPConfig) config.BackendHTTPConfig {
	t.Helper()
	full := config.Config{
		Server:  config.ServerConfig{ListenAddr: ":9000"},
		Buckets: []config.BucketConfig{{Name: "b", Credentials: []config.CredentialConfig{{AccessKeyID: "AKID", SecretAccessKey: "s"}}}},
		Backends: []config.BackendConfig{
			{Name: "b1", Endpoint: "https://e", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", HTTP: cfg},
		},
	}
	if err := full.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config did not validate: %v", err)
	}
	return full.Backends[0].HTTP
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestNewBackendTransport_UsesConfiguredPoolSizes asserts the block reaches the
// transport. One fixed setting either over-allocates file descriptors on a
// small box or starves throughput on a large one, which is the whole reason
// these are configurable.
func TestNewBackendTransport_UsesConfiguredPoolSizes(t *testing.T) {
	t.Parallel()
	tr := newBackendTransport(defaultedHTTPConfig(t, config.BackendHTTPConfig{
		MaxIdleConns:          7,
		MaxIdleConnsPerHost:   5,
		MaxConnsPerHost:       9,
		ResponseHeaderTimeout: 3 * time.Second,
	}))

	if tr.MaxIdleConns != 7 || tr.MaxIdleConnsPerHost != 5 || tr.MaxConnsPerHost != 9 {
		t.Errorf("pool sizes = %d/%d/%d, want 7/5/9",
			tr.MaxIdleConns, tr.MaxIdleConnsPerHost, tr.MaxConnsPerHost)
	}
	if tr.ResponseHeaderTimeout != 3*time.Second {
		t.Errorf("ResponseHeaderTimeout = %s, want 3s", tr.ResponseHeaderTimeout)
	}
}

// TestNewBackendTransport_DefaultsMatchThePreviousFixedValues holds that a
// config which never mentions the block is dialled exactly as it was before
// the block existed.
func TestNewBackendTransport_DefaultsMatchThePreviousFixedValues(t *testing.T) {
	t.Parallel()
	tr := newBackendTransport(defaultedHTTPConfig(t, config.BackendHTTPConfig{}))

	if tr.MaxIdleConns != 100 || tr.MaxIdleConnsPerHost != 100 || tr.MaxConnsPerHost != 200 {
		t.Errorf("pool sizes = %d/%d/%d, want the previous 100/100/200",
			tr.MaxIdleConns, tr.MaxIdleConnsPerHost, tr.MaxConnsPerHost)
	}
	if tr.ResponseHeaderTimeout != 30*time.Second {
		t.Errorf("ResponseHeaderTimeout = %s, want the previous 30s", tr.ResponseHeaderTimeout)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("HTTP/2 should still be attempted by default")
	}
}

// TestNewBackendTransport_HTTP2OptOut is the point of the feature: one backend
// drops to HTTP/1.1 without changing how any other is dialled, which is the
// targeted form of the process-wide GODEBUG workaround.
func TestNewBackendTransport_HTTP2OptOut(t *testing.T) {
	t.Parallel()
	off, on := false, true

	if tr := newBackendTransport(defaultedHTTPConfig(t, config.BackendHTTPConfig{ForceHTTP2: &off})); tr.ForceAttemptHTTP2 {
		t.Error("force_http2: false should stop the transport attempting HTTP/2")
	}
	if tr := newBackendTransport(defaultedHTTPConfig(t, config.BackendHTTPConfig{ForceHTTP2: &on})); !tr.ForceAttemptHTTP2 {
		t.Error("force_http2: true should attempt HTTP/2")
	}
}

// TestNewBackendTransport_FixedTimeoutsStayFixed pins what is deliberately not
// configurable: the dial and TLS-handshake bounds decide how long a broken
// endpoint can hold a request, which is not a per-deployment judgement.
func TestNewBackendTransport_FixedTimeoutsStayFixed(t *testing.T) {
	t.Parallel()
	tr := newBackendTransport(defaultedHTTPConfig(t, config.BackendHTTPConfig{ResponseHeaderTimeout: time.Second}))

	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout = %s, want 10s", tr.TLSHandshakeTimeout)
	}
	if tr.IdleConnTimeout != 60*time.Second {
		t.Errorf("IdleConnTimeout = %s, want 60s", tr.IdleConnTimeout)
	}
}
