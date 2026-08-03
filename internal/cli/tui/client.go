// -------------------------------------------------------------------------------
// TUI - Admin API Client
//
// Author: Alex Freidah
//
// Typed HTTP client over the admin API used by the browser. Unlike the adminctl
// CLI client, which renders output and returns exit codes, this one decodes and
// returns values so the Bubble Tea model can react to them as messages.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

const (
	adminTokenHeader = "X-Admin-Token"
	requestTimeout   = 30 * time.Second
)

// apiClient issues authenticated requests against the admin API and returns
// decoded responses.
type apiClient struct {
	baseAddr string
	token    string
	http     *http.Client
}

// newAPIClient builds a client for the resolved target address and token.
func newAPIClient(baseAddr, token string) *apiClient {
	return &apiClient{
		baseAddr: strings.TrimRight(baseAddr, "/"),
		token:    token,
		http:     &http.Client{Timeout: requestTimeout},
	}
}

// ListObjects fetches one delimiter-grouped page under prefix. A non-empty
// continuation resumes a previously truncated page.
func (c *apiClient) ListObjects(ctx context.Context, prefix, continuation string) (*adminapi.ObjectListResponse, error) {
	q := url.Values{}
	q.Set("prefix", prefix)
	q.Set("delimiter", "/")
	if continuation != "" {
		q.Set("continuation", continuation)
	}
	return getJSON[adminapi.ObjectListResponse](ctx, c, "/admin/api/objects", q)
}

// GetObjectLocations fetches every backend copy of a single object key.
func (c *apiClient) GetObjectLocations(ctx context.Context, key string) (*adminapi.ObjectLocationsResponse, error) {
	q := url.Values{}
	q.Set("key", key)
	return getJSON[adminapi.ObjectLocationsResponse](ctx, c, "/admin/api/object-locations", q)
}

// GetStatus fetches instance and per-backend operational status.
func (c *apiClient) GetStatus(ctx context.Context) (*adminapi.StatusResponse, error) {
	return getJSON[adminapi.StatusResponse](ctx, c, "/admin/api/status", nil)
}

// GetLogs fetches recent structured log entries from the in-memory buffer,
// filtered to the given minimum level (empty returns all levels).
func (c *apiClient) GetLogs(ctx context.Context, level string) (*adminapi.LogsResponse, error) {
	var q url.Values
	if level != "" {
		q = url.Values{"level": {level}}
	}
	return getJSON[adminapi.LogsResponse](ctx, c, "/admin/api/logs", q)
}

// GetReplicationStatus fetches the latest replication snapshot (factor and the
// current under- and over-replicated object counts) computed by the metrics
// collector. Returns an error until the first snapshot is available.
func (c *apiClient) GetReplicationStatus(ctx context.Context) (*adminapi.ReplicationStatusResponse, error) {
	return getJSON[adminapi.ReplicationStatusResponse](ctx, c, "/admin/api/replication", nil)
}

// GetWorkers fetches every registered background service's last-tick health.
// Returns an unavailable apiError on a proxy-only deployment, which registers
// no worker pool.
func (c *apiClient) GetWorkers(ctx context.Context) (*adminapi.WorkersResponse, error) {
	return getJSON[adminapi.WorkersResponse](ctx, c, "/admin/api/workers", nil)
}

// GetCleanupQueue fetches the pending-cleanup depth and a page of rows awaiting
// a successful backend delete.
func (c *apiClient) GetCleanupQueue(ctx context.Context) (*adminapi.CleanupQueueResponse, error) {
	return getJSON[adminapi.CleanupQueueResponse](ctx, c, "/admin/api/cleanup-queue", nil)
}

// GetCleanupDLQ fetches the dead-letter depth and a page of rows that exhausted
// their retry budget.
func (c *apiClient) GetCleanupDLQ(ctx context.Context) (*adminapi.CleanupDLQResponse, error) {
	return getJSON[adminapi.CleanupDLQResponse](ctx, c, "/admin/api/cleanup-dlq", nil)
}

// GetCacheStats fetches the object data cache's utilization. Returns an
// unavailable apiError when object caching is disabled.
func (c *apiClient) GetCacheStats(ctx context.Context) (*adminapi.CacheStatsResponse, error) {
	return getJSON[adminapi.CacheStatsResponse](ctx, c, "/admin/api/cache", nil)
}

// RequeueCleanupDLQ moves dead-lettered rows back into the cleanup queue,
// scoped to one backend when backend is non-empty.
func (c *apiClient) RequeueCleanupDLQ(ctx context.Context, backend string) (*adminapi.CleanupDLQRequeueResponse, error) {
	var q url.Values
	if backend != "" {
		q = url.Values{"backend": {backend}}
	}
	return postJSON[adminapi.CleanupDLQRequeueResponse](ctx, c, "/admin/api/cleanup-dlq/requeue", q)
}

// eventStream yields an admin operation's events in order, returning io.EOF when
// the operation is exhausted. Both the server's live NDJSON progress stream and
// the single synthesized result of a one-shot action satisfy it, so the Ops pane
// consumes every action the same way.
type eventStream interface {
	Next() (adminstream.Event, error)
	Close() error
}

// RunOp starts an admin instance action and returns its event stream. A
// long-running action opts into the server's NDJSON progress stream; a short
// one POSTs and has its summary decoded into a single result event, so the two
// render identically. A >=400 status becomes an error carrying the trimmed body.
func (c *apiClient) RunOp(ctx context.Context, act opsAction) (eventStream, error) {
	stream := act.result == nil

	req, err := http.NewRequestWithContext(ctx, act.method, c.baseAddr+act.path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(adminTokenHeader, c.token)
	if stream {
		req.Header.Set("Accept", adminstream.ContentType)
	}

	// A stream outlives any fixed deadline (the server keeps it active with
	// progress events), so it runs on a client with no timeout; one-shot
	// actions use the shared timeout client.
	httpClient := c.http
	if stream {
		httpClient = &http.Client{}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		err := readAPIError(resp)
		_ = resp.Body.Close()
		return nil, err
	}
	if stream {
		return &decoderStream{dec: json.NewDecoder(resp.Body), body: resp.Body}, nil
	}
	defer resp.Body.Close()
	event, err := act.result(resp.Body)
	if err != nil {
		return nil, err
	}
	return &sliceStream{events: []adminstream.Event{event}}, nil
}

// decoderStream adapts the live NDJSON body to eventStream.
type decoderStream struct {
	dec  *json.Decoder
	body io.Closer
}

func (s *decoderStream) Next() (adminstream.Event, error) {
	var e adminstream.Event
	err := s.dec.Decode(&e)
	return e, err
}

func (s *decoderStream) Close() error { return s.body.Close() }

// sliceStream replays a fixed set of events (a one-shot action's single result).
type sliceStream struct {
	events []adminstream.Event
	i      int
}

func (s *sliceStream) Next() (adminstream.Event, error) {
	if s.i >= len(s.events) {
		return adminstream.Event{}, io.EOF
	}
	e := s.events[s.i]
	s.i++
	return e, nil
}

func (s *sliceStream) Close() error { return nil }

// apiError is a non-2xx admin API response. It keeps the status code so a
// caller can tell a genuine failure apart from an endpoint the deployment
// simply does not offer: /admin/api/workers answers 503 on a proxy-only
// instance, and every /admin/api/cache route answers 503 when caching is off.
type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	if d := e.detail(); d != "" {
		return fmt.Sprintf("admin API returned %d: %s", e.status, d)
	}
	return fmt.Sprintf("admin API returned %d", e.status)
}

// unavailable reports whether the endpoint is absent by configuration rather
// than broken.
func (e *apiError) unavailable() bool { return e.status == http.StatusServiceUnavailable }

// detail extracts the human-readable part of an error body, which the admin
// API writes as either {"error":...} or, for a disabled subsystem,
// {"status":"disabled","reason":...}. Falls back to the raw body so an
// unrecognised shape is still legible.
func (e *apiError) detail() string {
	var parsed struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(e.body), &parsed) == nil {
		if parsed.Error != "" {
			return parsed.Error
		}
		if parsed.Reason != "" {
			return parsed.Reason
		}
	}
	return e.body
}

// unavailableReason returns the explanation for an endpoint the deployment does
// not offer, or "" when err is any other kind of failure. Panes use it to
// render a configuration notice instead of an error.
func unavailableReason(err error) string {
	var apiErr *apiError
	if !errors.As(err, &apiErr) || !apiErr.unavailable() {
		return ""
	}
	if d := apiErr.detail(); d != "" {
		return d
	}
	return "not available on this instance"
}

// getJSON issues an authenticated GET against the admin API and decodes the
// response body into T.
func getJSON[T any](ctx context.Context, c *apiClient, path string, q url.Values) (*T, error) {
	return doJSON[T](ctx, c, http.MethodGet, path, q)
}

// postJSON issues an authenticated POST against the admin API and decodes the
// response body into T. Every admin action the browser posts takes its
// arguments in the query string, so there is no request body.
func postJSON[T any](ctx context.Context, c *apiClient, path string, q url.Values) (*T, error) {
	return doJSON[T](ctx, c, http.MethodPost, path, q)
}

// doJSON issues an authenticated request and decodes the response into T. A
// >=400 status becomes an *apiError carrying the status and trimmed body.
func doJSON[T any](ctx context.Context, c *apiClient, method, path string, q url.Values) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseAddr+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(adminTokenHeader, c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, readAPIError(resp)
	}

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// readAPIError drains an error response into an *apiError.
func readAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return &apiError{status: resp.StatusCode, body: strings.TrimSpace(string(body))}
}
