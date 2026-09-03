// -------------------------------------------------------------------------------
// Admin API Client - Shared Transport
//
// Author: Alex Freidah
//
// The HTTP half of talking to the admin API, shared by every out-of-process
// consumer: request construction, token auth, the deadline split between
// one-shot calls and progress streams, and the typed non-2xx error.
//
// The wire shapes already live in one place (transport/admin/adminapi) so the
// server and its clients cannot disagree about JSON. This package is the same
// argument applied to the transport: adminctl and the TUI previously carried
// their own copies of all of it, including two independently written parsers
// for the same error body.
//
// Presentation stays with the caller. This package returns values and errors;
// it never renders, never writes to a terminal, and never chooses an exit code.
// -------------------------------------------------------------------------------

package adminclient

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// TokenHeader carries the admin token on every request.
const TokenHeader = "X-Admin-Token"

// RequestTimeout bounds a one-shot admin call. Streams deliberately run
// without one: the server flushes a progress event per batch, so the
// connection stays active for the life of the operation and cancellation is
// the caller's context, not a deadline.
const RequestTimeout = 30 * time.Second

// Client issues authenticated requests against one admin API instance.
type Client struct {
	baseAddr string
	token    string
	http     *http.Client
	stream   *http.Client // deadline-free; see RequestTimeout
}

// New builds a client for the resolved target address and token. A trailing
// slash on addr is tolerated so callers can pass an operator-supplied value
// through unmodified.
func New(addr, token string) *Client {
	return &Client{
		baseAddr: strings.TrimRight(addr, "/"),
		token:    token,
		http:     &http.Client{Timeout: RequestTimeout},
		stream:   &http.Client{},
	}
}

// Do issues an authenticated request and returns the raw response, which the
// caller owns and must close. Non-2xx statuses come back as a live response,
// not an error: callers that want the typed form use JSON or Stream.
func (c *Client) Do(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Response, error) {
	return c.send(ctx, c.http, method, path, q, body, "")
}

// Stream issues a request that opts into the server's NDJSON progress stream
// and returns the events in order. A non-2xx status becomes an *Error.
func (c *Client) Stream(ctx context.Context, method, path string, q url.Values, body io.Reader) (EventStream, error) {
	resp, err := c.send(ctx, c.stream, method, path, q, body, adminstream.ContentType)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		err := readError(resp)
		_ = resp.Body.Close()
		return nil, err
	}
	return newDecoderStream(resp.Body), nil
}

// Upload issues an authenticated request whose body is raw bytes rather than a
// JSON document, declaring the length the endpoint needs to accept it. The
// caller owns and must close the returned response.
func (c *Client) Upload(
	ctx context.Context,
	method, path string,
	q url.Values,
	body io.Reader,
	size int64,
	contentType string,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseAddr+path+queryOf(q), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(TokenHeader, c.token)
	req.Header.Set("Content-Type", contentType)
	// A streamed body has no length of its own, and the endpoint refuses an
	// upload it cannot size, so it is declared here.
	req.ContentLength = size
	//nolint:gosec // G704: the target address is operator-supplied by design.
	return c.http.Do(req)
}

// send builds and dispatches one request. accept, when non-empty, sets the
// Accept header; a non-nil body is sent as JSON.
func (c *Client) send(
	ctx context.Context,
	httpClient *http.Client,
	method, path string,
	q url.Values,
	body io.Reader,
	accept string,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseAddr+path+queryOf(q), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(TokenHeader, c.token)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	//nolint:gosec // G704: the target address is operator-supplied by design.
	return httpClient.Do(req)
}

// queryOf renders a query string, including the leading "?" only when there is
// something to encode, so paths without parameters stay byte-identical to what
// the caller passed.
func queryOf(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}
