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
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

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
	q := url.Values{}
	q.Set("prefix", prefix)
	q.Set("delimiter", "/")
	if continuation != "" {
		q.Set("continuation", continuation)
	}
	return adminclient.Get[adminapi.ObjectListResponse](ctx, c.c, "/admin/api/objects", q)
}

// GetObjectLocations fetches every backend copy of a single object key.
func (c *apiClient) GetObjectLocations(ctx context.Context, key string) (*adminapi.ObjectLocationsResponse, error) {
	q := url.Values{}
	q.Set("key", key)
	return adminclient.Get[adminapi.ObjectLocationsResponse](ctx, c.c, "/admin/api/object-locations", q)
}

// GetStatus fetches instance and per-backend operational status.
func (c *apiClient) GetStatus(ctx context.Context) (*adminapi.StatusResponse, error) {
	return adminclient.Get[adminapi.StatusResponse](ctx, c.c, "/admin/api/status", nil)
}

// GetLogs fetches recent structured log entries from the in-memory buffer,
// filtered to the given minimum level (empty returns all levels).
func (c *apiClient) GetLogs(ctx context.Context, level string) (*adminapi.LogsResponse, error) {
	var q url.Values
	if level != "" {
		q = url.Values{"level": {level}}
	}
	return adminclient.Get[adminapi.LogsResponse](ctx, c.c, "/admin/api/logs", q)
}

// GetReplicationStatus fetches the latest replication snapshot (factor and the
// current under- and over-replicated object counts) computed by the metrics
// collector. Returns an error until the first snapshot is available.
func (c *apiClient) GetReplicationStatus(ctx context.Context) (*adminapi.ReplicationStatusResponse, error) {
	return adminclient.Get[adminapi.ReplicationStatusResponse](ctx, c.c, "/admin/api/replication", nil)
}

// GetWorkers fetches every registered background service's last-tick health.
// Returns an unavailable adminclient.Error on a proxy-only deployment, which registers
// no worker pool.
func (c *apiClient) GetWorkers(ctx context.Context) (*adminapi.WorkersResponse, error) {
	return adminclient.Get[adminapi.WorkersResponse](ctx, c.c, "/admin/api/workers", nil)
}

// GetCleanupQueue fetches the pending-cleanup depth and a page of rows awaiting
// a successful backend delete.
func (c *apiClient) GetCleanupQueue(ctx context.Context) (*adminapi.CleanupQueueResponse, error) {
	return adminclient.Get[adminapi.CleanupQueueResponse](ctx, c.c, "/admin/api/cleanup-queue", nil)
}

// GetCleanupDLQ fetches the dead-letter depth and a page of rows that exhausted
// their retry budget.
func (c *apiClient) GetCleanupDLQ(ctx context.Context) (*adminapi.CleanupDLQResponse, error) {
	return adminclient.Get[adminapi.CleanupDLQResponse](ctx, c.c, "/admin/api/cleanup-dlq", nil)
}

// GetCacheStats fetches the object data cache's utilization. Returns an
// unavailable adminclient.Error when object caching is disabled.
func (c *apiClient) GetCacheStats(ctx context.Context) (*adminapi.CacheStatsResponse, error) {
	return adminclient.Get[adminapi.CacheStatsResponse](ctx, c.c, "/admin/api/cache", nil)
}

// RequeueCleanupDLQ moves dead-lettered rows back into the cleanup queue,
// scoped to one backend when backend is non-empty.
func (c *apiClient) RequeueCleanupDLQ(ctx context.Context, backend string) (*adminapi.CleanupDLQRequeueResponse, error) {
	var q url.Values
	if backend != "" {
		q = url.Values{"backend": {backend}}
	}
	return adminclient.Post[adminapi.CleanupDLQRequeueResponse](ctx, c.c, "/admin/api/cleanup-dlq/requeue", q, nil)
}

// RunOp starts an admin instance action and returns its event stream. A
// long-running action opts into the server's NDJSON progress stream; a short
// one POSTs and has its summary decoded into a single result event, so the two
// render identically.
func (c *apiClient) RunOp(ctx context.Context, act opsAction) (adminclient.EventStream, error) {
	if act.result == nil {
		return c.c.Stream(ctx, act.method, act.path, nil, nil)
	}

	resp, err := c.c.Do(ctx, act.method, act.path, nil, nil)
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
