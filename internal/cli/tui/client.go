// -------------------------------------------------------------------------------
// TUI - Admin API Client
//
// Author: Alex Freidah
//
// The browser's view of the admin API: one method per endpoint, each returning
// a decoded value so the Bubble Tea model can react to it as a message. The
// HTTP half - auth, deadlines, NDJSON, typed errors - is shared with adminctl
// in internal/cli/adminclient.
// -------------------------------------------------------------------------------

package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// objectsPath is the object namespace endpoint, named so the listing, the
// prefix delete, and the per-object paths built from it cannot drift.
const objectsPath = "/admin/api/objects"

// apiClient issues authenticated requests against the admin API and returns
// decoded responses.
type apiClient struct {
	c *adminclient.Client
}

// newAPIClient builds a client for the resolved target address and token.
func newAPIClient(baseAddr, token string) *apiClient {
	return &apiClient{c: adminclient.New(baseAddr, token)}
}

// ListObjects fetches one delimiter-grouped page under prefix. A non-empty
// continuation resumes a previously truncated page.
func (c *apiClient) ListObjects(ctx context.Context, prefix, continuation string) (*adminapi.ObjectListResponse, error) {
	return c.listObjects(ctx, prefix, "/", continuation)
}

// listObjects fetches one page under prefix. The delimiter is always sent,
// including empty: omitting it asks for a hierarchical listing, so the flat
// listing has to say so explicitly rather than leave it out.
func (c *apiClient) listObjects(ctx context.Context, prefix, delimiter, continuation string) (*adminapi.ObjectListResponse, error) {
	q := url.Values{}
	q.Set("prefix", prefix)
	q.Set("delimiter", delimiter)
	if continuation != "" {
		q.Set("continuation", continuation)
	}
	return c.c.Get[adminapi.ObjectListResponse](ctx, objectsPath, q)
}

// GetObjectLocations fetches every backend copy of a single object key.
func (c *apiClient) GetObjectLocations(ctx context.Context, key string) (*adminapi.ObjectLocationsResponse, error) {
	q := url.Values{}
	q.Set("key", key)
	return c.c.Get[adminapi.ObjectLocationsResponse](ctx, "/admin/api/object-locations", q)
}

// GetObjectTags fetches one object's tag set. The key rides in the path rather
// than a query parameter, matching the admin route.
func (c *apiClient) GetObjectTags(ctx context.Context, key string) (*adminapi.ObjectTagsResponse, error) {
	return c.c.Get[adminapi.ObjectTagsResponse](ctx, "/admin/api/objects/tags/"+url.PathEscape(key), nil)
}

// ScrubKey verifies every recorded copy of one key now and reports a verdict
// per copy.
func (c *apiClient) ScrubKey(ctx context.Context, key string) (*adminapi.ScrubKeyResponse, error) {
	q := url.Values{}
	q.Set("key", key)
	return c.c.Post[adminapi.ScrubKeyResponse](ctx, "/admin/api/object-scrub", q, nil)
}

// GetStatus fetches instance and per-backend operational status.
func (c *apiClient) GetStatus(ctx context.Context) (*adminapi.StatusResponse, error) {
	return c.c.Get[adminapi.StatusResponse](ctx, "/admin/api/status", nil)
}

// GetLogs fetches recent structured log entries from the in-memory buffer,
// filtered to the given minimum level (empty returns all levels).
func (c *apiClient) GetLogs(ctx context.Context, level string) (*adminapi.LogsResponse, error) {
	var q url.Values
	if level != "" {
		q = url.Values{"level": {level}}
	}
	return c.c.Get[adminapi.LogsResponse](ctx, "/admin/api/logs", q)
}

// GetReplicationStatus fetches the latest replication snapshot (factor and the
// current under- and over-replicated object counts) computed by the metrics
// collector. Returns an error until the first snapshot is available.
func (c *apiClient) GetReplicationStatus(ctx context.Context) (*adminapi.ReplicationStatusResponse, error) {
	return c.c.Get[adminapi.ReplicationStatusResponse](ctx, "/admin/api/replication", nil)
}

// GetWorkers fetches every registered background service's last-tick health.
// Returns an unavailable adminclient.Error on a proxy-only deployment, which registers
// no worker pool.
func (c *apiClient) GetWorkers(ctx context.Context) (*adminapi.WorkersResponse, error) {
	return c.c.Get[adminapi.WorkersResponse](ctx, "/admin/api/workers", nil)
}

// GetCleanupQueue fetches the pending-cleanup depth and a page of rows awaiting
// a successful backend delete.
func (c *apiClient) GetCleanupQueue(ctx context.Context) (*adminapi.CleanupQueueResponse, error) {
	return c.c.Get[adminapi.CleanupQueueResponse](ctx, "/admin/api/cleanup-queue", nil)
}

// GetCleanupDLQ fetches the dead-letter depth and a page of rows that exhausted
// their retry budget.
func (c *apiClient) GetCleanupDLQ(ctx context.Context) (*adminapi.CleanupDLQResponse, error) {
	return c.c.Get[adminapi.CleanupDLQResponse](ctx, "/admin/api/cleanup-dlq", nil)
}

// GetCacheStats fetches the object data cache's utilization. Returns an
// unavailable adminclient.Error when object caching is disabled.
func (c *apiClient) GetCacheStats(ctx context.Context) (*adminapi.CacheStatsResponse, error) {
	return c.c.Get[adminapi.CacheStatsResponse](ctx, "/admin/api/cache", nil)
}

// RequeueCleanupDLQ moves dead-lettered rows back into the cleanup queue,
// scoped to one backend when backend is non-empty.
func (c *apiClient) RequeueCleanupDLQ(ctx context.Context, backend string) (*adminapi.CleanupDLQRequeueResponse, error) {
	var q url.Values
	if backend != "" {
		q = url.Values{"backend": {backend}}
	}
	return c.c.Post[adminapi.CleanupDLQRequeueResponse](ctx, "/admin/api/cleanup-dlq/requeue", q, nil)
}

// ListObjectsFlat fetches one page of every key under prefix, ungrouped.
func (c *apiClient) ListObjectsFlat(ctx context.Context, prefix, continuation string) (*adminapi.ObjectListResponse, error) {
	return c.listObjects(ctx, prefix, "", continuation)
}

// DownloadObject streams one object and returns the open body alongside its
// size, so a caller can report progress against a total. The caller closes the
// body.
func (c *apiClient) DownloadObject(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := c.c.Do(ctx, http.MethodGet, objectPath(key), nil, nil)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, 0, &adminclient.Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return resp.Body, resp.ContentLength, nil
}

// UploadObject stores size bytes read from body under key.
func (c *apiClient) UploadObject(ctx context.Context, key string, body io.Reader, size int64) error {
	resp, err := c.c.Upload(ctx, http.MethodPut, objectPath(key), nil, body, size, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		payload, _ := io.ReadAll(resp.Body)
		return &adminclient.Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(payload))}
	}
	return nil
}

// DeleteObject removes one object and every copy of it.
func (c *apiClient) DeleteObject(ctx context.Context, key string) (*adminapi.ObjectDeleteResponse, error) {
	return c.deleteJSON(ctx, objectPath(key), nil)
}

// DeletePrefix removes every object under prefix and reports how many it
// removed.
func (c *apiClient) DeletePrefix(ctx context.Context, prefix string) (*adminapi.ObjectDeleteResponse, error) {
	return c.deleteJSON(ctx, objectsPath, url.Values{"prefix": {prefix}})
}

// deleteJSON issues a DELETE and decodes the JSON summary, which the shared
// helpers do not cover since they only wrap GET and POST.
func (c *apiClient) deleteJSON(ctx context.Context, path string, q url.Values) (*adminapi.ObjectDeleteResponse, error) {
	resp, err := c.c.Do(ctx, http.MethodDelete, path, q, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return nil, &adminclient.Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var out adminapi.ObjectDeleteResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// objectPath is the per-object endpoint. Keys contain slashes, which the
// wildcard route accepts verbatim.
func objectPath(key string) string {
	return objectsPath + "/" + key
}

// StartDrain begins migrating every copy off one backend and routes new writes
// away from it. Returns as soon as the drain is accepted; progress is polled
// with DrainProgress.
func (c *apiClient) StartDrain(ctx context.Context, backend string) (*adminapi.BackendOperationResponse, error) {
	return c.c.Post[adminapi.BackendOperationResponse](ctx, backendDrainPath(backend), nil, nil)
}

// DrainProgress reports how far an in-flight drain has got. Active is false
// once the migration finished, was cancelled, or never started.
func (c *apiClient) DrainProgress(ctx context.Context, backend string) (*adminapi.DrainProgressResponse, error) {
	return c.c.Get[adminapi.DrainProgressResponse](ctx, backendDrainPath(backend), nil)
}

// CancelDrain aborts an in-flight drain. Copies already migrated stay migrated.
func (c *apiClient) CancelDrain(ctx context.Context, backend string) (*adminapi.BackendOperationResponse, error) {
	resp, err := c.c.Do(ctx, http.MethodDelete, backendDrainPath(backend), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return nil, &adminclient.Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var out adminapi.BackendOperationResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReconcileBackend reconciles metadata against one backend's storage rather
// than the whole fleet.
func (c *apiClient) ReconcileBackend(ctx context.Context, backend string) (*adminapi.ReconcileResponse, error) {
	return c.c.Post[adminapi.ReconcileResponse](ctx, "/admin/api/reconcile",
		url.Values{"backend": {backend}}, nil)
}

// backendDrainPath is the drain endpoint for one backend, shared by the three
// verbs so the path cannot drift between them.
func backendDrainPath(backend string) string {
	return "/admin/api/backends/" + backend + "/drain"
}

// RunOp starts an admin instance action and returns its event stream. A
// long-running action opts into the server's NDJSON progress stream; a short
// one sends its request and has the summary decoded into a single result
// event, so the two render identically. req carries the path, query and body
// the action resolved to, which is how the actions that take an operator-typed
// value reach the endpoint.
func (c *apiClient) RunOp(ctx context.Context, act *opsAction, req opsRequest) (adminclient.EventStream, error) {
	if act.result == nil {
		return c.c.Stream(ctx, act.method, req.path, req.query, bodyReader(req.body))
	}

	resp, err := c.c.Do(ctx, act.method, req.path, req.query, bodyReader(req.body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return nil, &adminclient.Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}

	event, err := act.result(resp.Body)
	if err != nil {
		return nil, err
	}
	return adminclient.NewSliceStream(event), nil
}

// bodyReader wraps a request body, or reports none so the send path stays a
// single call for both shapes.
func bodyReader(body []byte) io.Reader {
	if len(body) == 0 {
		return nil
	}
	return bytes.NewReader(body)
}
