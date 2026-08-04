// -------------------------------------------------------------------------------
// Admin API Client - Transport Tests
//
// Author: Alex Freidah
//
// Pins the transport contracts both consumers depend on but neither states:
// how auth and content headers are set, that a query string is omitted rather
// than left empty, that Do hands back a non-2xx response alive while the typed
// helpers convert it, and that streams opt into NDJSON. These decisions are
// load-bearing - adminctl renders raw error bodies in JSON mode, and the TUI
// distinguishes a disabled endpoint from a broken one - so a change to any of
// them should fail here rather than in a distant consumer assertion.
// -------------------------------------------------------------------------------

package adminclient

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// capture records what the server saw, so header and path assertions read as
// one struct rather than a fistful of closures.
type capture struct {
	method      string
	path        string
	rawQuery    string
	token       string
	accept      string
	contentType string
	body        string
}

// newCaptureServer returns a server that records the request and replies with
// the given status and body.
func newCaptureServer(t *testing.T, status int, body string) (*httptest.Server, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = capture{
			method:      r.Method,
			path:        r.URL.Path,
			rawQuery:    r.URL.RawQuery,
			token:       r.Header.Get(TokenHeader),
			accept:      r.Header.Get("Accept"),
			contentType: r.Header.Get("Content-Type"),
			body:        string(b),
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// TestNew_TrimsTrailingSlash asserts an operator-supplied address with a
// trailing slash does not produce a double slash in the request path.
func TestNew_TrimsTrailingSlash(t *testing.T) {
	t.Parallel()
	srv, got := newCaptureServer(t, http.StatusOK, `{}`)

	c := New(srv.URL+"/", "tok")
	resp, err := c.Do(t.Context(), http.MethodGet, "/admin/api/status", nil, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got.path != "/admin/api/status" {
		t.Errorf("path = %q, want /admin/api/status", got.path)
	}
}

// TestDo_SetsTokenAndOmitsEmptyQuery asserts the auth header is always present
// and that a nil query yields no "?" at all, so paths stay byte-identical to
// what the caller passed.
func TestDo_SetsTokenAndOmitsEmptyQuery(t *testing.T) {
	t.Parallel()
	srv, got := newCaptureServer(t, http.StatusOK, `{}`)

	c := New(srv.URL, "tok")
	resp, err := c.Do(t.Context(), http.MethodGet, "/admin/api/status", nil, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got.token != "tok" {
		t.Errorf("token header = %q, want tok", got.token)
	}
	if got.rawQuery != "" {
		t.Errorf("raw query = %q, want empty", got.rawQuery)
	}
	// A request without a body must not claim to carry JSON.
	if got.contentType != "" {
		t.Errorf("content-type = %q, want empty for a bodyless request", got.contentType)
	}
	// Do is not the streaming path and must not ask for the event stream.
	if got.accept == adminstream.ContentType {
		t.Error("Do set the NDJSON Accept header; that belongs to Stream")
	}
}

// TestDo_EncodesQueryAndBody covers the two optional request parts together.
func TestDo_EncodesQueryAndBody(t *testing.T) {
	t.Parallel()
	srv, got := newCaptureServer(t, http.StatusOK, `{}`)

	c := New(srv.URL, "tok")
	resp, err := c.Do(t.Context(), http.MethodPost, "/admin/api/x",
		url.Values{"backend": {"b1"}}, strings.NewReader(`{"k":"v"}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got.rawQuery != "backend=b1" {
		t.Errorf("raw query = %q, want backend=b1", got.rawQuery)
	}
	if got.body != `{"k":"v"}` {
		t.Errorf("body = %q", got.body)
	}
	if got.contentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", got.contentType)
	}
}

// TestDo_ReturnsNonOKResponseAlive is the contract adminctl depends on: a
// non-2xx must come back as a readable response, not an error, because JSON
// mode pretty-prints the server's raw error body.
func TestDo_ReturnsNonOKResponseAlive(t *testing.T) {
	t.Parallel()
	srv, _ := newCaptureServer(t, http.StatusForbidden, `{"error":"denied"}`)

	c := New(srv.URL, "tok")
	resp, err := c.Do(t.Context(), http.MethodGet, "/admin/api/status", nil, nil)
	if err != nil {
		t.Fatalf("Do returned an error for a 403; the raw body must stay readable: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"error":"denied"}` {
		t.Errorf("body = %q, want the server's raw error body", body)
	}
}

// TestGet_DecodesAndTypesErrors covers the typed path: the happy case decodes
// into T, and a non-2xx becomes an *Error carrying status and body - the
// opposite of Do's contract, and what the TUI relies on.
func TestGet_DecodesAndTypesErrors(t *testing.T) {
	t.Parallel()

	t.Run("decodes", func(t *testing.T) {
		t.Parallel()
		srv, got := newCaptureServer(t, http.StatusOK, `{"entries":3,"hits":30}`)
		type stats struct {
			Entries int `json:"entries"`
			Hits    int `json:"hits"`
		}
		out, err := Get[stats](t.Context(), New(srv.URL, "tok"), "/admin/api/cache", nil)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if out.Entries != 3 || out.Hits != 30 {
			t.Errorf("decoded %+v", out)
		}
		if got.method != http.MethodGet {
			t.Errorf("method = %s, want GET", got.method)
		}
	})

	t.Run("types a non-2xx", func(t *testing.T) {
		t.Parallel()
		srv, _ := newCaptureServer(t, http.StatusServiceUnavailable, `{"status":"disabled","reason":"caching is off"}`)
		_, err := Get[struct{}](t.Context(), New(srv.URL, "tok"), "/admin/api/cache", nil)

		apiErr, ok := errors.AsType[*Error](err)
		if !ok {
			t.Fatalf("err = %v (%T), want *Error", err, err)
		}
		if apiErr.Status != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", apiErr.Status)
		}
		if got := UnavailableReason(err); got != "caching is off" {
			t.Errorf("UnavailableReason = %q", got)
		}
	})

	t.Run("surfaces a decode failure", func(t *testing.T) {
		t.Parallel()
		srv, _ := newCaptureServer(t, http.StatusOK, `{not json`)
		if _, err := Get[struct{}](t.Context(), New(srv.URL, "tok"), "/admin/api/cache", nil); err == nil {
			t.Error("expected a decode error")
		}
	})
}

// TestPost_UsesPOST asserts the verb, since every admin action is a POST and a
// silent downgrade to GET would hit a different route.
func TestPost_UsesPOST(t *testing.T) {
	t.Parallel()
	srv, got := newCaptureServer(t, http.StatusOK, `{"requeued":4}`)

	type resp struct {
		Requeued int `json:"requeued"`
	}
	out, err := Post[resp](t.Context(), New(srv.URL, "tok"), "/admin/api/cleanup-dlq/requeue",
		url.Values{"backend": {"b1"}}, nil)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.rawQuery != "backend=b1" || out.Requeued != 4 {
		t.Errorf("query=%q requeued=%d", got.rawQuery, out.Requeued)
	}
}

// TestStream_OptsIntoNDJSONAndYieldsEventsInOrder covers the streaming path:
// the Accept header opts in, and events arrive in wire order terminated by
// io.EOF.
func TestStream_OptsIntoNDJSONAndYieldsEventsInOrder(t *testing.T) {
	t.Parallel()
	body := `{"kind":"step","message":"one"}` + "\n" + `{"kind":"result","outcome":"ok","message":"done"}` + "\n"
	srv, got := newCaptureServer(t, http.StatusOK, body)

	events, err := New(srv.URL, "tok").Stream(t.Context(), http.MethodPost, "/admin/api/scrub", nil, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer events.Close()

	if got.accept != adminstream.ContentType {
		t.Errorf("accept = %q, want %q", got.accept, adminstream.ContentType)
	}

	var messages []string
	for {
		e, err := events.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		messages = append(messages, e.Message)
	}
	if len(messages) != 2 || messages[0] != "one" || messages[1] != "done" {
		t.Errorf("messages = %v, want [one done]", messages)
	}
}

// TestStream_TypesANonOKStatus asserts a failed stream open reports the body,
// which adminctl renders instead of a bare transport error.
func TestStream_TypesANonOKStatus(t *testing.T) {
	t.Parallel()
	srv, _ := newCaptureServer(t, http.StatusInternalServerError, `{"error":"boom"}`)

	_, err := New(srv.URL, "tok").Stream(t.Context(), http.MethodPost, "/admin/api/scrub", nil, nil)
	apiErr, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("err = %v (%T), want *Error", err, err)
	}
	if apiErr.Status != http.StatusInternalServerError || apiErr.Detail() != "boom" {
		t.Errorf("err = %+v, want 500 / boom", apiErr)
	}
}

// TestStream_TransportFailure covers an unreachable target.
func TestStream_TransportFailure(t *testing.T) {
	t.Parallel()
	// Port 0 is never listening, so Do fails before any response exists.
	if _, err := New("http://127.0.0.1:0", "tok").Stream(t.Context(), http.MethodPost, "/x", nil, nil); err == nil {
		t.Error("expected a transport error")
	}
}

// TestSliceStream_ReplaysThenEOF covers the synthesized stream that lets a
// one-shot action render through the same path as a live one.
func TestSliceStream_ReplaysThenEOF(t *testing.T) {
	t.Parallel()
	s := NewSliceStream(
		adminstream.Event{Kind: adminstream.KindResult, Message: "only"},
	)
	defer s.Close()

	e, err := s.Next()
	if err != nil || e.Message != "only" {
		t.Fatalf("first Next = (%+v, %v)", e, err)
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("second Next err = %v, want io.EOF", err)
	}
}

// TestSliceStream_Empty asserts an empty stream is immediately exhausted
// rather than blocking or panicking.
func TestSliceStream_Empty(t *testing.T) {
	t.Parallel()
	if _, err := NewSliceStream().Next(); !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

// TestRequestTimeoutIsSetOnOneShotButNotStreams pins the deadline split. A
// stream outlives any fixed deadline because the server keeps it active with
// progress events; a one-shot call must not hang forever.
func TestRequestTimeoutIsSetOnOneShotButNotStreams(t *testing.T) {
	t.Parallel()
	c := New("http://example.invalid", "tok")
	if c.http.Timeout != RequestTimeout {
		t.Errorf("one-shot timeout = %v, want %v", c.http.Timeout, RequestTimeout)
	}
	if c.stream.Timeout != 0 {
		t.Errorf("stream timeout = %v, want none", c.stream.Timeout)
	}
}

// TestSend_RejectsAnUnbuildableRequest covers the request-construction failure
// branch: an invalid method never reaches the network, and the error surfaces
// rather than being swallowed into a nil response.
func TestSend_RejectsAnUnbuildableRequest(t *testing.T) {
	t.Parallel()
	c := New("http://example.invalid", "tok")
	// A method containing a space is not a valid HTTP token.
	resp, err := c.Do(t.Context(), "BAD METHOD", "/x", nil, nil)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected an error building the request")
	}
}

// TestGet_TransportFailure covers the typed path's transport branch, which is
// distinct from a non-2xx: there is no response to read a body from.
func TestGet_TransportFailure(t *testing.T) {
	t.Parallel()
	// Port 0 is never listening, so the round trip fails outright.
	_, err := Get[struct{}](t.Context(), New("http://127.0.0.1:0", "tok"), "/x", nil)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if _, ok := errors.AsType[*Error](err); ok {
		t.Error("a transport failure must not be reported as an admin API *Error")
	}
}
