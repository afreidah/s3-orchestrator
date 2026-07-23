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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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

// eventStream yields an admin operation's events in order, returning io.EOF when
// the operation is exhausted. Both the server's live NDJSON progress stream and
// the single synthesized result of a one-shot action satisfy it, so the Ops pane
// consumes every action the same way.
type eventStream interface {
	Next() (adminstream.Event, error)
	Close() error
}

// RunOp starts an admin instance action and returns its event stream. A
// streaming action opts into the server's NDJSON progress stream; a one-shot
// action POSTs and returns a single synthesized result event so the two render
// identically. A >=400 status becomes an error carrying the trimmed body.
func (c *apiClient) RunOp(ctx context.Context, method, path string, stream bool) (eventStream, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseAddr+path, nil)
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
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("admin API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if stream {
		return &decoderStream{dec: json.NewDecoder(resp.Body), body: resp.Body}, nil
	}
	defer resp.Body.Close()
	var summary map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		return nil, err
	}
	return &sliceStream{events: []adminstream.Event{oneShotResult(summary)}}, nil
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

// oneShotResult turns a non-streaming action's JSON summary into a single
// terminal result event so one-shot and streaming actions render alike.
func oneShotResult(summary map[string]any) adminstream.Event {
	status, _ := summary["status"].(string)
	outcome := adminstream.OutcomeOK
	if status == "skipped" || status == "disabled" {
		outcome = adminstream.OutcomeSkipped
	}
	msg := summarizeFields(summary)
	if outcome == adminstream.OutcomeSkipped {
		if reason, ok := summary["reason"].(string); ok && reason != "" {
			msg = reason
		}
	}
	if msg == "" {
		msg = status
	}
	return adminstream.Event{Kind: adminstream.KindResult, Outcome: outcome, Message: msg}
}

// summarizeFields renders a one-shot summary's detail fields as sorted
// key=value pairs, skipping the status and reason keys handled separately.
func summarizeFields(summary map[string]any) string {
	keys := make([]string, 0, len(summary))
	for k := range summary {
		if k == "status" || k == "reason" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, summary[k]))
	}
	return strings.Join(parts, " ")
}

// getJSON issues an authenticated GET against the admin API and decodes the
// response body into T. A >=400 status becomes an error carrying the trimmed
// response body.
func getJSON[T any](ctx context.Context, c *apiClient, path string, q url.Values) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseAddr+path+"?"+q.Encode(), nil)
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
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("admin API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
