// -------------------------------------------------------------------------------
// Admin CLI - HTTP Client and Response Rendering
//
// Author: Alex Freidah
//
// The transport seam shared by every admin subcommand. A client carries the
// resolved target, auth token, output format, and writers; its verb methods
// issue one authenticated request and render the outcome. JSON mode pretty-
// prints the server's raw bytes (byte-stable for scripts); text mode runs a
// per-command renderer, falling back to a generic key/value view.
// -------------------------------------------------------------------------------

package adminctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
)

const requestTimeout = 30 * time.Second

// client carries the resolved target, auth token, output format, and writers
// shared by every admin subcommand handler.
type client struct {
	baseAddr string
	token    string
	format   output.Format
	stdout   io.Writer
	stderr   io.Writer
}

// renderFunc renders a successful response body in text mode. A nil renderer
// falls back to the generic key/value renderer.
type renderFunc func(w io.Writer, body []byte) error

// get/post/put/delete issue the named verb against path (appended to the
// resolved base address) and render the response per the client's format.
func (c *client) get(path string, render renderFunc) int {
	return c.do(http.MethodGet, path, "", render)
}

func (c *client) post(path, body string, render renderFunc) int {
	return c.do(http.MethodPost, path, body, render)
}

func (c *client) put(path, body string, render renderFunc) int {
	return c.do(http.MethodPut, path, body, render)
}

func (c *client) delete(path string, render renderFunc) int {
	return c.do(http.MethodDelete, path, "", render)
}

// do performs one admin request and renders the outcome. Transport and read
// errors are reported to stderr and return exit code 1; an HTTP status >= 400
// renders the error and returns 1; otherwise the body renders per format.
func (c *client) do(method, path, body string, render renderFunc) int {
	data, status, code := c.request(method, path, body)
	if code != 0 {
		return code
	}
	if status >= 400 {
		c.renderError(data)
		return 1
	}
	c.renderSuccess(data, render)
	return 0
}

// request issues an authenticated request and returns the response body, the
// HTTP status code, and an exit code (non-zero when a transport or read error
// was already reported to stderr). Sets the X-Admin-Token header for auth, the
// Content-Type header when a body is present, and a fixed client timeout so a
// hung server cannot stall the CLI indefinitely.
func (c *client) request(method, path, body string) ([]byte, int, int) {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, c.baseAddr+path, bodyReader)
	if err != nil {
		fmt.Fprintf(c.stderr, fmtError, err)
		return nil, 0, 1
	}
	req.Header.Set(adminTokenHeader, c.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{Timeout: requestTimeout}).Do(req) //nolint:gosec // G704: admin CLI target address is user-provided via --addr flag
	if err != nil {
		fmt.Fprintf(c.stderr, fmtError, err)
		return nil, 0, 1
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(c.stderr, "error reading response: %v\n", err)
		return nil, 0, 1
	}
	return data, resp.StatusCode, 0
}

// renderSuccess writes a successful response body. JSON mode pretty-prints the
// server's raw bytes; text mode runs the supplied renderer, falling back to
// the generic key/value view and, on render failure, to the raw JSON so output
// is never lost.
func (c *client) renderSuccess(data []byte, render renderFunc) {
	if c.format.IsJSON() {
		_ = output.PrettyJSON(c.stdout, data)
		return
	}
	if render == nil {
		render = output.RenderValue
	}
	if err := render(c.stdout, data); err != nil {
		_ = output.PrettyJSON(c.stdout, data)
	}
}

// renderError writes an error response body. JSON mode pretty-prints the raw
// bytes; text mode prints a single "error: <message>" line to stderr.
func (c *client) renderError(data []byte) {
	if c.format.IsJSON() {
		_ = output.PrettyJSON(c.stdout, data)
		return
	}
	fmt.Fprintf(c.stderr, "error: %s\n", errorMessage(data))
}

// fetchJSON issues an authenticated request and decodes the JSON body into a
// map. Returns (body, exitCode); a non-zero exitCode means the helper already
// reported an error to stderr and the caller should propagate it. Shared by
// the remove-backend preview and purge flows, which each carry only the
// response-shape handling unique to them.
func (c *client) fetchJSON(method, path string) (map[string]any, int) {
	req, err := http.NewRequestWithContext(context.Background(), method, c.baseAddr+path, nil)
	if err != nil {
		fmt.Fprintf(c.stderr, fmtError, err)
		return nil, 1
	}
	req.Header.Set(adminTokenHeader, c.token)

	resp, err := (&http.Client{Timeout: requestTimeout}).Do(req) //nolint:gosec // G704: admin CLI target address is user-provided via --addr flag
	if err != nil {
		fmt.Fprintf(c.stderr, fmtError, err)
		return nil, 1
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(c.stderr, "error: failed to parse response: %v\n", err)
		return nil, 1
	}
	return result, 0
}

// errorMessage extracts the "error" field from a JSON error body, falling back
// to the trimmed raw body when the response is not the expected shape.
func errorMessage(data []byte) string {
	var m map[string]any
	if json.Unmarshal(data, &m) == nil {
		if e, ok := m["error"].(string); ok && e != "" {
			return e
		}
	}
	return strings.TrimSpace(string(data))
}
