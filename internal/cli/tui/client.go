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
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
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

// ReconcileUsage recomputes every backend's bytes_used from the object ledger.
func (c *apiClient) ReconcileUsage(ctx context.Context) error {
	return c.doAdmin(ctx, http.MethodPost, "/admin/api/usage-reconcile")
}

// FlushCache clears the in-memory object cache.
func (c *apiClient) FlushCache(ctx context.Context) error {
	return c.doAdmin(ctx, http.MethodPost, "/admin/api/cache/flush")
}

// doAdmin issues an authenticated write (POST/DELETE) against the admin API,
// returning an error carrying the trimmed body on a >=400 status. The response
// body is otherwise discarded - action callers only need success or failure.
func (c *apiClient) doAdmin(ctx context.Context, method, path string) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseAddr+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set(adminTokenHeader, c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("admin API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
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
