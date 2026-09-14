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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// TokenHeader carries the admin token on every request.
const TokenHeader = "X-Admin-Token"

// RequestTimeout bounds a one-shot admin call. Streams deliberately run
// without one: the server flushes a progress event per batch, so the
// connection stays active for the life of the operation and cancellation is
// the caller's context, not a deadline.
const RequestTimeout = 30 * time.Second

// signingRegion is the scope a signed admin request is signed under. The server
// takes the access key from the credential scope and verifies against whatever
// scope the header carries, so this only has to match between the two halves of
// one request.
const signingRegion = "us-east-1"

// contentSHAHeader carries the payload hash the signature was built over.
const contentSHAHeader = "X-Amz-Content-Sha256"

// unsignedPayload is the SigV4 sentinel for a body whose hash is not computed,
// which is what an upload streams: hashing it would mean buffering the object.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// keypair is the credential a client signs with, when it signs.
type keypair struct {
	accessKeyID string
	secretKey   string
}

// Client issues authenticated requests against one admin API instance.
//
// Exactly one of token or keys proves the caller. Signing is the path an
// operator should be on; the token remains for a deployment that has not moved
// off it yet.
type Client struct {
	baseAddr string
	token    string
	keys     *keypair
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

// NewSigned builds a client that signs each request with SigV4, which is the
// same credential the S3 API accepts.
func NewSigned(addr, accessKeyID, secretKey string) *Client {
	c := New(addr, "")
	c.keys = &keypair{accessKeyID: accessKeyID, secretKey: secretKey}
	return c
}

// authorize proves the request, by signature where the client holds a keypair
// and by token otherwise.
//
// payload is the body's SHA-256, or unsignedPayload for a streamed one. A
// signature covers the headers it names, so this runs after every other header
// is set.
func (c *Client) authorize(ctx context.Context, req *http.Request, payload string) error {
	if c.keys == nil {
		req.Header.Set(TokenHeader, c.token)
		return nil
	}
	creds := aws.Credentials{
		AccessKeyID:     c.keys.accessKeyID,
		SecretAccessKey: c.keys.secretKey,
	}
	// The server reads the payload hash from this header and falls back to
	// UNSIGNED-PAYLOAD when it is absent, so a body hashed into the signature
	// has to be declared here or the two sides canonicalise differently and
	// every signed request is refused. Set before signing, so it is covered.
	req.Header.Set(contentSHAHeader, payload)
	return v4.NewSigner().SignHTTP(ctx, creds, req, payload, "s3", signingRegion, time.Now().UTC())
}

// hashOf renders the SHA-256 a signature covers the body with.
func hashOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
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
	req.Header.Set("Content-Type", contentType)
	// A streamed body has no length of its own, and the endpoint refuses an
	// upload it cannot size, so it is declared here.
	req.ContentLength = size
	// The body is not hashed: an object is streamed, and hashing it would mean
	// holding the whole upload to sign it.
	if err := c.authorize(ctx, req, unsignedPayload); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}
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
	// Read the body up front so the signature can cover it. These carry names
	// and small JSON documents, never object data, so holding one is cheap and
	// it is what lets the request be retried or signed at all.
	var payload []byte
	if body != nil {
		var err error
		if payload, err = io.ReadAll(body); err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseAddr+path+queryOf(q), body)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(payload))
	}
	if err := c.authorize(ctx, req, hashOf(payload)); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
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
