// -------------------------------------------------------------------------------
// Integration Tests - Backend Proxy
//
// Author: Alex Freidah
//
// Every harness backend is reached through a proxy in front of its MinIO
// endpoint, so a test can change how the backend answers without touching
// MinIO: drop Range from every request the way some S3 gateways do, refuse a
// request, or carry a request out and lose the reply.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// proxyAction is what a proxy rule does with one request.
type proxyAction int

// A rule passes a request on, answers 501 without passing it on, or passes it
// on and answers 501 in place of MinIO's reply. 501 is a server error the S3
// client does not retry, so a refused request fails at once.
const (
	proxyPass proxyAction = iota
	proxyRefuse
	proxyLoseReply
)

// proxyRule picks the action for one request on its way to MinIO. A test
// installs one with SetRule and clears it with nil.
type proxyRule func(r *http.Request) proxyAction

// backendProxy fronts one MinIO endpoint and puts every request to its rule.
// The lock covers the rule, which a test swaps while requests are in flight.
type backendProxy struct {
	proxy *httputil.ReverseProxy
	mu    sync.Mutex
	rule  proxyRule
}

// newBackendProxy starts a proxy in front of endpoint and returns it with the
// URL a backend config points at. With ignoreRange it drops Range from every
// request.
func newBackendProxy(t *testing.T, endpoint string, ignoreRange bool) (*backendProxy, string) {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse MinIO endpoint %s: %v", endpoint, err)
	}
	p := &backendProxy{proxy: httputil.NewSingleHostReverseProxy(target)}
	if ignoreRange {
		p.dropRange(t)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, srv.URL
}

// dropRange removes Range from every request, so MinIO answers each ranged GET
// with the whole object. SigV4 signs Range, so a request that loses it is
// signed again with the harness credentials.
func (p *backendProxy) dropRange(t *testing.T) {
	// S3 signs the path as sent, without the second escaping SigV4 applies to
	// other services; the stored keys carry "!" and would not match otherwise.
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: "minioadmin", SecretAccessKey: "minioadmin"}
	direct := p.proxy.Director
	p.proxy.Director = func(r *http.Request) {
		direct(r)
		if r.Header.Get("Range") == "" {
			return
		}
		r.Header.Del("Range")
		r.Header.Del("Authorization")
		payloadHash := r.Header.Get("X-Amz-Content-Sha256")
		if err := signer.SignHTTP(r.Context(), creds, r, payloadHash, "s3", "us-east-1", time.Now()); err != nil {
			t.Errorf("re-sign request without Range: %v", err)
		}
	}
}

// SetRule installs the rule every later request is put to, replacing the one
// before it. A nil rule passes every request on.
func (p *backendProxy) SetRule(rule proxyRule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rule = rule
}

// ServeHTTP answers one request as the installed rule says. A lost reply is
// carried out against MinIO in full before the 501 goes back.
func (p *backendProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch p.action(r) {
	case proxyRefuse:
		http.Error(w, "refused by the test", http.StatusNotImplemented)
	case proxyLoseReply:
		p.proxy.ServeHTTP(httptest.NewRecorder(), r)
		http.Error(w, "reply lost by the test", http.StatusNotImplemented)
	default:
		p.proxy.ServeHTTP(w, r)
	}
}

// action returns what the installed rule does with r, passing it on when no
// rule is installed.
func (p *backendProxy) action(r *http.Request) proxyAction {
	p.mu.Lock()
	rule := p.rule
	p.mu.Unlock()
	if rule == nil {
		return proxyPass
	}
	return rule(r)
}
