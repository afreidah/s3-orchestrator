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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseAddr+"/admin/api/objects?"+q.Encode(), nil)
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

	var out adminapi.ObjectListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
