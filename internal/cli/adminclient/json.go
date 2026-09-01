// -------------------------------------------------------------------------------
// Admin API Client - Typed JSON Calls
//
// Author: Alex Freidah
//
// Generic helpers that issue a request and decode the response into one of the
// adminapi wire types. Free functions rather than methods because Go does not
// allow type parameters on methods.
// -------------------------------------------------------------------------------

package adminclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
)

// Get issues an authenticated GET and decodes the response into T.
func (c *Client) Get[T any](ctx context.Context, path string, q url.Values) (*T, error) {
	return c.do[T](ctx, http.MethodGet, path, q, nil)
}

// Post issues an authenticated POST and decodes the response into T. Most
// admin actions take their arguments in the query string, so body is usually
// nil.
func (c *Client) Post[T any](ctx context.Context, path string, q url.Values, body io.Reader) (*T, error) {
	return c.do[T](ctx, http.MethodPost, path, q, body)
}

// do issues a request and decodes the response into T. A >=400 status becomes
// an *Error carrying the status and trimmed body.
func (c *Client) do[T any](ctx context.Context, method, path string, q url.Values, body io.Reader) (*T, error) {
	resp, err := c.send(ctx, c.http, method, path, q, body, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, readError(resp)
	}

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
