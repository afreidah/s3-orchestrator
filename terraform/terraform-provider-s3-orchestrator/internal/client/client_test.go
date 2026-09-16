// -------------------------------------------------------------------------------
// Admin API Client - Transport Tests
//
// Author: Alex Freidah
//
// Covers request construction, signing and the typed error a refusal becomes,
// against a stub server rather than a real orchestrator. What matters here is
// the shape of what goes out and what a caller can tell from what comes back;
// that a real deployment accepts the signature is the acceptance tests' job.
// -------------------------------------------------------------------------------

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// stub serves one handler and returns a client pointed at it.
func stub(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "AKIATESTCLIENT000000", "test-secret")
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

func TestNewNormalisesAddress(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		addr string
		want string
	}{
		{"bare host and port defaults to http", "localhost:9000", "http://localhost:9000"},
		{"http is preserved", "http://example.test", "http://example.test"},
		{"https is preserved", "https://example.test", "https://example.test"},
		{"trailing slash is trimmed", "http://example.test/", "http://example.test"},
		{"scheme is added before the slash is trimmed", "example.test/", "http://example.test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := New(tc.addr, "k", "s").baseAddr; got != tc.want {
				t.Errorf("baseAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDoSignsOverThePayload is the one that keeps the two halves of a request
// agreeing. The server reads the payload hash from the content header, so a
// body hashed into the signature but not declared there canonicalises
// differently on each side and every request is refused.
func TestDoSignsOverThePayload(t *testing.T) {
	t.Parallel()
	var gotAuth, gotHash, gotType, gotBody string
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotAuth, gotHash = r.Header.Get("Authorization"), r.Header.Get(contentSHAHeader)
		gotType, gotBody = r.Header.Get("Content-Type"), string(raw)
		w.WriteHeader(http.StatusOK)
	})

	if err := c.Do(context.Background(), http.MethodPost, "/x", CreateUserRequest{Name: "n"}, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}

	sum := sha256.Sum256([]byte(gotBody))
	if want := hex.EncodeToString(sum[:]); gotHash != want {
		t.Errorf("payload hash = %q, want %q", gotHash, want)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization = %q, want a SigV4 signature", gotAuth)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
	if gotBody != `{"name":"n"}` {
		t.Errorf("body = %q, want the encoded request", gotBody)
	}
}

// TestDoWithoutBodySendsNone covers a nil body arriving as no bytes rather than
// as the JSON null a plain Marshal would produce.
func TestDoWithoutBodySendsNone(t *testing.T) {
	t.Parallel()
	var gotBody, gotType string
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody, gotType = string(raw), r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.Do(context.Background(), http.MethodDelete, "/x", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotBody != "" {
		t.Errorf("body = %q, want empty", gotBody)
	}
	if gotType != "" {
		t.Errorf("Content-Type = %q, want unset", gotType)
	}
}

func TestDoDecodesResponse(t *testing.T) {
	t.Parallel()
	c := stub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok","user_id":"user-abc"}`)
	})

	var out OperationResponse
	if err := c.Do(context.Background(), http.MethodGet, "/x", nil, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out.Status != "ok" || out.UserID != "user-abc" {
		t.Errorf("decoded %+v, want status ok and user user-abc", out)
	}
}

func TestDoRejectsUndecodableResponse(t *testing.T) {
	t.Parallel()
	c := stub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `not json`)
	})

	var out OperationResponse
	err := c.Do(context.Background(), http.MethodGet, "/x", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v, want a decode failure", err)
	}
}

// TestDoTypesRefusals covers what a caller reads off a non-2xx, which is what
// decides whether a Read drops the resource, reports a conflict, or says the
// entry is declared in the configuration file.
func TestDoTypesRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		status    int
		body      string
		wantMsg   string
		predicate func(*Error) bool
	}{
		{
			name: "not found", status: http.StatusNotFound,
			body: `{"error":"user not found"}`, wantMsg: "admin API returned 404: user not found",
			predicate: (*Error).NotFound,
		},
		{
			name: "conflict", status: http.StatusConflict,
			body: `{"error":"name taken"}`, wantMsg: "admin API returned 409: name taken",
			predicate: (*Error).Conflict,
		},
		{
			name: "forbidden", status: http.StatusForbidden,
			body: `{"error":"declared in config"}`, wantMsg: "admin API returned 403: declared in config",
			predicate: (*Error).Forbidden,
		},
		{
			// A proxy refusing the request before it arrives answers with
			// HTML, and swallowing that leaves a caller with a bare status.
			name: "a non-JSON refusal keeps its body", status: http.StatusBadGateway,
			body: "<html>bad gateway</html>", wantMsg: "admin API returned 502: <html>bad gateway</html>",
			predicate: func(e *Error) bool { return e.Status == http.StatusBadGateway },
		},
		{
			name: "an empty refusal renders the status alone", status: http.StatusInternalServerError,
			body: "", wantMsg: "admin API returned 500",
			predicate: func(e *Error) bool { return e.Message == "" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := stub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})

			err := c.Do(context.Background(), http.MethodGet, "/x", nil, nil)
			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want an *Error", err)
			}
			if apiErr.Error() != tc.wantMsg {
				t.Errorf("message = %q, want %q", apiErr.Error(), tc.wantMsg)
			}
			if !tc.predicate(apiErr) {
				t.Errorf("%+v did not satisfy the case's predicate", apiErr)
			}
		})
	}
}

// TestDoReportsTransportFailure covers an orchestrator that is not there, which
// is an ordinary state for a provider run against a deployment still coming up.
func TestDoReportsTransportFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	c := New(srv.URL, "k", "s")
	srv.Close()

	err := c.Do(context.Background(), http.MethodGet, "/x", nil, nil)
	if err == nil {
		t.Fatal("err = nil, want a transport failure")
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		t.Errorf("err = %v, want a transport failure rather than an API refusal", err)
	}
}

func TestDoRejectsUnencodableBody(t *testing.T) {
	t.Parallel()
	c := New("http://example.test", "k", "s")

	err := c.Do(context.Background(), http.MethodPost, "/x", make(chan int), nil)
	if err == nil || !strings.Contains(err.Error(), "encode request") {
		t.Fatalf("err = %v, want an encode failure", err)
	}
}

func TestDoRejectsUnusableMethod(t *testing.T) {
	t.Parallel()
	c := New("http://example.test", "k", "s")

	err := c.Do(context.Background(), "in valid", "/x", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "build request") {
		t.Fatalf("err = %v, want a request-construction failure", err)
	}
}
