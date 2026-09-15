// -------------------------------------------------------------------------------
// Admin API Client - Transport
//
// Author: Alex Freidah
//
// The HTTP half of talking to a running s3-orchestrator: request construction,
// SigV4 signing, and the typed error a non-2xx becomes. The provider is an
// ordinary client of the admin API rather than anything privileged, so it signs
// with a keypair the deployment holds exactly as the CLI and the dashboard do.
//
// The wire types the orchestrator owns are restated here rather than imported:
// they live under internal/ in that module and cannot be reached from this one.
// Restating them also pins the shape this provider was built against, which is
// what a published provider owes its pinned consumers.
// -------------------------------------------------------------------------------

package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// signingRegion is the scope a signed request is signed under. The server takes
// the access key from the credential scope and verifies against whatever scope
// the header carries, so this only has to match between the two halves of one
// request.
const signingRegion = "us-east-1"

// signingService is the SigV4 service name, matching what the S3 surface signs
// under so one credential works on both.
const signingService = "s3"

// contentSHAHeader carries the payload hash the signature was built over.
const contentSHAHeader = "X-Amz-Content-Sha256"

// requestTimeout bounds one call. Provisioning calls exchange names and small
// JSON documents and rebuild the registry before returning, so this is generous
// rather than tight.
const requestTimeout = 30 * time.Second

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Client talks to one orchestrator's admin API.
type Client struct {
	baseAddr string
	keys     aws.Credentials
	http     *http.Client
}

// Error is a non-2xx answer, carrying enough for a caller to tell a missing
// resource from one it may not touch.
type Error struct {
	Status  int
	Message string
}

// Error renders the status and whatever the server said about it.
func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("admin API returned %d", e.Status)
	}
	return fmt.Sprintf("admin API returned %d: %s", e.Status, e.Message)
}

// NotFound reports whether the resource does not exist, which is what tells a
// Read to drop the resource from state rather than fail the run.
func (e *Error) NotFound() bool { return e.Status == http.StatusNotFound }

// Conflict reports whether something already claims the name or still depends
// on what was being removed.
func (e *Error) Conflict() bool { return e.Status == http.StatusConflict }

// Forbidden reports whether the entry is declared in the orchestrator's config
// file. Those are visible through the API and never editable through it, so a
// caller is told that rather than left reading it as a credential problem.
func (e *Error) Forbidden() bool { return e.Status == http.StatusForbidden }

// errorBody is the shape every refusal answers with.
type errorBody struct {
	Error string `json:"error"`
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// New builds a client signing as the given keypair.
//
// A bare host:port defaults to http, because the common target is an instance
// reached over a loopback or a private network. A caller pointing at a remote
// one supplies the scheme, and https is preserved exactly.
func New(addr, accessKeyID, secretAccessKey string) *Client {
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr // NOSONAR S5332: scheme default for an operator-supplied address
	}
	return &Client{
		baseAddr: strings.TrimSuffix(addr, "/"),
		keys: aws.Credentials{
			AccessKeyID:     accessKeyID,
			SecretAccessKey: secretAccessKey,
		},
		http: &http.Client{Timeout: requestTimeout},
	}
}

// Do issues one signed request, decoding a 2xx body into out when out is not
// nil. A non-2xx becomes an *Error carrying the status.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	payload, err := encode(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseAddr+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(payload))
	}
	if err := c.sign(ctx, req, payload); err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return &Error{Status: resp.StatusCode, Message: messageOf(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// sign covers the request with a SigV4 signature.
//
// The server reads the payload hash from the content header and falls back to
// the unsigned sentinel when it is absent, so a body hashed into the signature
// has to be declared there or the two sides canonicalise differently and every
// request is refused. Set before signing, so it is covered.
func (c *Client) sign(ctx context.Context, req *http.Request, payload []byte) error {
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	req.Header.Set(contentSHAHeader, hash)
	if err := v4.NewSigner().SignHTTP(
		ctx, c.keys, req, hash, signingService, signingRegion, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}
	return nil
}

// encode renders a request body, treating a nil body as no bytes rather than
// as the JSON null a plain Marshal would produce.
func encode(body any) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return raw, nil
}

// messageOf pulls the message out of an error body, falling back to the raw
// bytes when the answer is not the JSON shape the API documents. A proxy
// refusing the request before it arrives answers with HTML, and swallowing that
// would leave a caller with a bare status code.
func messageOf(raw []byte) string {
	var e errorBody
	if err := json.Unmarshal(raw, &e); err == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(raw))
}
