// -------------------------------------------------------------------------------
// Backend Tests - Literal Path
//
// Author: Alex Freidah
//
// Drives S3Backend against a fake provider that refuses a path with !'()*
// escaped, as one whose signature check leaves them literal does, and records
// every path and body it is sent. Covers learning the literal form from the
// first refusal, resending a PUT's body, a streamed PUT that cannot be resent,
// a provider that signs to spec, and a refusal the literal form does not cure.
// -------------------------------------------------------------------------------

package backend

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// escapedSubDelim matches an escaped !'()* in a request path.
var escapedSubDelim = regexp.MustCompile(`%(21|27|28|29|2A)`)

// literalProvider is a fake S3 endpoint that refuses a path with !'()* escaped
// with a 403, unless acceptEscaped says it signs to spec, and refuses every
// request when refuseAll is set. paths and bodies record what it was sent.
type literalProvider struct {
	acceptEscaped bool
	refuseAll     bool

	mu     sync.Mutex
	paths  []string
	bodies []string
}

// ServeHTTP records the request and answers it.
func (p *literalProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path, _, _ := strings.Cut(r.RequestURI, "?")
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	p.paths = append(p.paths, path)
	p.bodies = append(p.bodies, string(body))
	p.mu.Unlock()

	if p.refuseAll || (escapedSubDelim.MatchString(path) && !p.acceptEscaped) {
		writeS3Error(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut:
		w.Header().Set("ETag", `"ok"`)
	default:
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	}
}

// sent returns a snapshot of the request paths the provider was sent.
func (p *literalProvider) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.paths)
}

// newLiteralBackend points an S3Backend at p, signing payloads unless
// unsigned is set.
func newLiteralBackend(t *testing.T, p *literalProvider, unsigned bool) *S3Backend {
	t.Helper()
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	be, err := NewS3Backend(t.Context(), &config.BackendConfig{
		Name: "literal", Endpoint: srv.URL, Region: "us-east-1", Bucket: "b",
		AccessKeyID: "AKID", SecretAccessKey: "secret",
		ForcePathStyle: true, DisableChecksum: true, UnsignedPayload: new(unsigned),
	})
	if err != nil {
		t.Fatalf("NewS3Backend: %v", err)
	}
	return be
}

// TestLiteralPath_LearnsFromTheFirstRefusal verifies the first refused escaped
// path is resent literal and answered, the backend is marked, and every later
// path is sent literal from the start while a plain one is left alone.
func TestLiteralPath_LearnsFromTheFirstRefusal(t *testing.T) {
	t.Parallel()
	p := &literalProvider{}
	be := newLiteralBackend(t, p, false)
	ctx := context.Background()

	if _, err := be.HeadObject(ctx, "k!w1"); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if !be.Unsupported().Has(core.FeatureEscapedPath) {
		t.Fatal("backend not marked after the literal form was taken")
	}
	got, err := be.GetObject(ctx, "a(b)*'c", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	_ = got.Body.Close()
	if err := be.DeleteObject(ctx, "plain"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	want := []string{"/b/k%21w1", "/b/k!w1", "/b/a(b)*'c", "/b/plain"}
	if sent := p.sent(); !slices.Equal(sent, want) {
		t.Errorf("paths = %v, want %v", sent, want)
	}
}

// TestLiteralPath_ResendsAPutWithItsBody verifies a refused PUT is resent with
// its whole body rewound.
func TestLiteralPath_ResendsAPutWithItsBody(t *testing.T) {
	t.Parallel()
	p := &literalProvider{}
	be := newLiteralBackend(t, p, false)

	if _, err := be.PutObject(context.Background(), "k!w1", bytes.NewReader([]byte("payload")), 7, "", nil); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if want := []string{"payload", "payload"}; !slices.Equal(p.bodies, want) {
		t.Errorf("bodies = %q, want the payload on both attempts", p.bodies)
	}
}

// TestLiteralPath_StreamedPutIsNotResent verifies a PUT whose body cannot be
// rewound fails as the provider answered without using up the one resend, so
// the next request learns the literal form.
func TestLiteralPath_StreamedPutIsNotResent(t *testing.T) {
	t.Parallel()
	p := &literalProvider{}
	be := newLiteralBackend(t, p, true)
	ctx := context.Background()

	stream := struct{ io.Reader }{strings.NewReader("payload")}
	if _, err := be.PutObject(ctx, "k!w1", stream, 7, "", nil); statusOf(err) != http.StatusForbidden {
		t.Fatalf("PutObject: err = %v, want the 403", err)
	}
	if be.Unsupported().Has(core.FeatureEscapedPath) || be.literalTried.Load() {
		t.Fatal("a streamed PUT that was not resent marked the backend or used the resend")
	}
	if _, err := be.HeadObject(ctx, "k!w1"); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if !be.Unsupported().Has(core.FeatureEscapedPath) {
		t.Error("the next request did not learn the literal form")
	}
}

// TestLiteralPath_SpecProviderIsSentItEscaped verifies a provider that signs
// to spec is only ever sent the escaped path.
func TestLiteralPath_SpecProviderIsSentItEscaped(t *testing.T) {
	t.Parallel()
	p := &literalProvider{acceptEscaped: true}
	be := newLiteralBackend(t, p, false)

	if _, err := be.HeadObject(context.Background(), "k!w1"); err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if sent := p.sent(); !slices.Equal(sent, []string{"/b/k%21w1"}) || be.Unsupported().Has(core.FeatureEscapedPath) {
		t.Errorf("paths = %v, marked = %t; want one escaped request and no mark",
			sent, be.Unsupported().Has(core.FeatureEscapedPath))
	}
}

// TestLiteralPath_OtherRefusalIsResentOnce verifies a provider that refuses
// the literal form too is resent it once, never marks the backend, and is not
// resent it again.
func TestLiteralPath_OtherRefusalIsResentOnce(t *testing.T) {
	t.Parallel()
	p := &literalProvider{refuseAll: true}
	be := newLiteralBackend(t, p, false)
	ctx := context.Background()

	for _, key := range []string{"k!w1", "k!w2"} {
		if _, err := be.HeadObject(ctx, key); statusOf(err) != http.StatusForbidden {
			t.Fatalf("HeadObject %s: err = %v, want the 403", key, err)
		}
	}
	if want := []string{"/b/k%21w1", "/b/k!w1", "/b/k%21w2"}; !slices.Equal(p.sent(), want) {
		t.Errorf("paths = %v, want %v", p.sent(), want)
	}
	if be.Unsupported().Has(core.FeatureEscapedPath) {
		t.Error("a refusal the literal form did not cure marked the backend")
	}
}
