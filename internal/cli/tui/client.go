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

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// apiClient issues authenticated requests against the admin API and returns
// decoded responses.
type apiClient struct {
	c *adminclient.Client
}

// newAPIClient builds a signing client for the resolved target.
func newAPIClient(t target) *apiClient {
	return &apiClient{c: adminclient.NewSigned(t.baseAddr, t.accessKeyID, t.secretKey)}
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
	return c.c.Get[adminapi.ObjectListResponse](ctx, adminapi.PathObjects, q)
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// GetObjectLocations fetches every backend copy of a single object key.
func (c *apiClient) GetObjectLocations(ctx context.Context, key string) (*adminapi.ObjectLocationsResponse, error) {
	q := url.Values{}
	q.Set("key", key)
	return c.c.Get[adminapi.ObjectLocationsResponse](ctx, adminapi.PathObjectLocations, q)
}

// GetObjectTags fetches one object's tag set. The key rides in the path rather
// than a query parameter, matching the admin route.
func (c *apiClient) GetObjectTags(ctx context.Context, key string) (*adminapi.ObjectTagsResponse, error) {
	return c.c.Get[adminapi.ObjectTagsResponse](ctx, adminapi.Path(adminapi.PathObjectTags, key), nil)
}

// SetObjectTags replaces one object's whole tag set and returns the set the
// server stored.
func (c *apiClient) SetObjectTags(ctx context.Context, key string, tags []adminapi.ObjectTag) (*adminapi.ObjectTagsResponse, error) {
	return c.c.Put[adminapi.ObjectTagsResponse](ctx, adminapi.Path(adminapi.PathObjectTags, key), nil,
		adminapi.ObjectTagsRequest{Tags: tags})
}

// DeleteObjectTags clears one object's tag set.
func (c *apiClient) DeleteObjectTags(ctx context.Context, key string) (*adminapi.ObjectTagsResponse, error) {
	return c.c.Delete[adminapi.ObjectTagsResponse](ctx, adminapi.Path(adminapi.PathObjectTags, key), nil)
}

// ScrubKey verifies every recorded copy of one key now and reports a verdict
// per copy.
func (c *apiClient) ScrubKey(ctx context.Context, key string) (*adminapi.ScrubKeyResponse, error) {
	q := url.Values{}
	q.Set("key", key)
	return c.c.Post[adminapi.ScrubKeyResponse](ctx, adminapi.PathObjectScrub, q, nil)
}

// GetStatus fetches instance and per-backend operational status.
func (c *apiClient) GetStatus(ctx context.Context) (*adminapi.StatusResponse, error) {
	return c.c.Get[adminapi.StatusResponse](ctx, adminapi.PathStatus, nil)
}

// GetLogs fetches recent structured log entries from the in-memory buffer,
// filtered to the given minimum level (empty returns all levels).
func (c *apiClient) GetLogs(ctx context.Context, level string) (*adminapi.LogsResponse, error) {
	var q url.Values
	if level != "" {
		q = url.Values{"level": {level}}
	}
	return c.c.Get[adminapi.LogsResponse](ctx, adminapi.PathLogs, q)
}

// GetReplicationStatus fetches the latest replication snapshot (factor and the
// current under- and over-replicated object counts) computed by the metrics
// collector. Returns an error until the first snapshot is available.
func (c *apiClient) GetReplicationStatus(ctx context.Context) (*adminapi.ReplicationStatusResponse, error) {
	return c.c.Get[adminapi.ReplicationStatusResponse](ctx, adminapi.PathReplication, nil)
}

// GetWorkers fetches every registered background service's last-tick health.
// Returns an unavailable adminclient.Error on a proxy-only deployment, which registers
// no worker pool.
func (c *apiClient) GetWorkers(ctx context.Context) (*adminapi.WorkersResponse, error) {
	return c.c.Get[adminapi.WorkersResponse](ctx, adminapi.PathWorkers, nil)
}

// GetConfig fetches the running configuration, with secrets redacted.
func (c *apiClient) GetConfig(ctx context.Context) (*adminapi.ConfigResponse, error) {
	return c.c.Get[adminapi.ConfigResponse](ctx, adminapi.PathConfig, nil)
}

// GetCleanupQueue fetches the pending-cleanup depth and a page of rows awaiting
// a successful backend delete.
func (c *apiClient) GetCleanupQueue(ctx context.Context) (*adminapi.CleanupQueueResponse, error) {
	return c.c.Get[adminapi.CleanupQueueResponse](ctx, adminapi.PathCleanupQueue, nil)
}

// GetCleanupDLQ fetches the dead-letter depth and a page of rows that exhausted
// their retry budget.
func (c *apiClient) GetCleanupDLQ(ctx context.Context) (*adminapi.CleanupDLQResponse, error) {
	return c.c.Get[adminapi.CleanupDLQResponse](ctx, adminapi.PathCleanupDLQ, nil)
}

// GetCacheStats fetches the object data cache's utilization. Returns an
// unavailable adminclient.Error when object caching is disabled.
func (c *apiClient) GetCacheStats(ctx context.Context) (*adminapi.CacheStatsResponse, error) {
	return c.c.Get[adminapi.CacheStatsResponse](ctx, adminapi.PathCache, nil)
}

// GetProvisioning fetches the virtual buckets, identities and credentials a
// deployment declares, from the config file and the store together.
func (c *apiClient) GetProvisioning(ctx context.Context) (*adminapi.ProvisioningResponse, error) {
	return c.c.Get[adminapi.ProvisioningResponse](ctx, adminapi.PathProvisioning, nil)
}

// RequeueCleanupDLQ moves dead-lettered rows back into the cleanup queue,
// scoped to one backend when backend is non-empty.
func (c *apiClient) RequeueCleanupDLQ(ctx context.Context, backend string) (*adminapi.CleanupDLQRequeueResponse, error) {
	var q url.Values
	if backend != "" {
		q = url.Values{"backend": {backend}}
	}
	return c.c.Post[adminapi.CleanupDLQRequeueResponse](ctx, adminapi.PathCleanupDLQRequeue, q, nil)
}

// ListObjectsFlat fetches one page of every key under prefix, ungrouped.
func (c *apiClient) ListObjectsFlat(ctx context.Context, prefix, continuation string) (*adminapi.ObjectListResponse, error) {
	return c.listObjects(ctx, prefix, "", continuation)
}

// DownloadObject streams one object and returns the open body alongside its
// size, so a caller can report progress against a total. The caller closes the
// body.
func (c *apiClient) DownloadObject(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := c.c.Do(ctx, http.MethodGet, adminapi.Path(adminapi.PathObjects, key), nil, nil)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer resp.Body.Close()
		return nil, 0, adminclient.ReadError(resp)
	}
	return resp.Body, resp.ContentLength, nil
}

// UploadObject stores size bytes read from body under key.
func (c *apiClient) UploadObject(ctx context.Context, key string, body io.Reader, size int64) error {
	resp, err := c.c.Upload(ctx, http.MethodPut, adminapi.Path(adminapi.PathObjects, key), nil, body, size, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return adminclient.ReadError(resp)
	}
	return nil
}

// DeleteObject removes one object and every copy of it.
func (c *apiClient) DeleteObject(ctx context.Context, key string) (*adminapi.ObjectDeleteResponse, error) {
	return c.c.Delete[adminapi.ObjectDeleteResponse](ctx, adminapi.Path(adminapi.PathObjects, key), nil)
}

// DeletePrefix removes every object under prefix and reports how many it
// removed.
func (c *apiClient) DeletePrefix(ctx context.Context, prefix string) (*adminapi.ObjectDeleteResponse, error) {
	return c.c.Delete[adminapi.ObjectDeleteResponse](ctx, adminapi.PathObjects, url.Values{"prefix": {prefix}})
}

// StartDrain begins migrating every copy off one backend and routes new writes
// away from it. Returns as soon as the drain is accepted; progress is polled
// with DrainProgress.
func (c *apiClient) StartDrain(ctx context.Context, backend string) (*adminapi.BackendOperationResponse, error) {
	return c.c.Post[adminapi.BackendOperationResponse](ctx, drainPath(backend), nil, nil)
}

// DrainProgress reports how far an in-flight drain has got. Active is false
// once the migration finished, was cancelled, or never started.
func (c *apiClient) DrainProgress(ctx context.Context, backend string) (*adminapi.DrainProgressResponse, error) {
	return c.c.Get[adminapi.DrainProgressResponse](ctx, drainPath(backend), nil)
}

// CancelDrain aborts an in-flight drain. Copies already migrated stay migrated.
func (c *apiClient) CancelDrain(ctx context.Context, backend string) (*adminapi.BackendOperationResponse, error) {
	return c.c.Delete[adminapi.BackendOperationResponse](ctx, drainPath(backend), nil)
}

// SetCordon cordons the backend when cordoned is true, stopping new copies
// landing on it, and uncordons it when false.
func (c *apiClient) SetCordon(ctx context.Context, backend string, cordoned bool) (*adminapi.BackendOperationResponse, error) {
	path := adminapi.Path(adminapi.PathBackends, backend) + adminapi.SubpathCordon
	if cordoned {
		return c.c.Post[adminapi.BackendOperationResponse](ctx, path, nil, nil)
	}
	return c.c.Delete[adminapi.BackendOperationResponse](ctx, path, nil)
}

// ReconcileBackend reconciles metadata against one backend's storage rather
// than the whole fleet.
func (c *apiClient) ReconcileBackend(ctx context.Context, backend string) (*adminapi.ReconcileResponse, error) {
	return c.c.Post[adminapi.ReconcileResponse](ctx, adminapi.PathReconcile,
		url.Values{"backend": {backend}}, nil)
}

// drainPath is the drain endpoint every drain verb shares.
func drainPath(backend string) string {
	return adminapi.Path(adminapi.PathBackends, backend) + adminapi.SubpathDrain
}

// RunOp starts an admin instance action and returns its event stream. A
// long-running action opts into the server's NDJSON progress stream; a short
// one sends its request and has the summary decoded into a single result
// event, so the two render identically. req carries the resolved path, query
// and body.
func (c *apiClient) RunOp(ctx context.Context, act *opsAction, req opsRequest) (adminclient.EventStream, error) {
	if act.result == nil {
		return c.c.Stream(ctx, act.method, req.path, req.query, req.body)
	}

	resp, err := c.c.Do(ctx, act.method, req.path, req.query, req.body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, adminclient.ReadError(resp)
	}

	event, err := act.result(resp.Body)
	if err != nil {
		return nil, err
	}
	return adminclient.NewSliceStream(event), nil
}
