// -------------------------------------------------------------------------------
// Admin API Client - Typed JSON Calls
//
// Author: Alex Freidah
//
// Generic methods that issue a request and decode the response into one of the
// adminapi wire types.
// -------------------------------------------------------------------------------

package adminclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Get issues an authenticated GET and decodes the response into T.
func (c *Client) Get[T any](ctx context.Context, path string, q url.Values) (*T, error) {
	return c.do[T](ctx, http.MethodGet, path, q, nil)
}

// Post issues an authenticated POST and decodes the response into T. Most
// admin actions take their arguments in the query string, so req is usually
// nil.
func (c *Client) Post[T any](ctx context.Context, path string, q url.Values, req any) (*T, error) {
	return c.do[T](ctx, http.MethodPost, path, q, req)
}

// Put issues an authenticated PUT carrying req as its JSON body and decodes
// the response into T.
func (c *Client) Put[T any](ctx context.Context, path string, q url.Values, req any) (*T, error) {
	return c.do[T](ctx, http.MethodPut, path, q, req)
}

// Delete issues an authenticated DELETE and decodes the response into T.
func (c *Client) Delete[T any](ctx context.Context, path string, q url.Values) (*T, error) {
	return c.do[T](ctx, http.MethodDelete, path, q, nil)
}

// do issues a request carrying req as its JSON body, or none when req is nil,
// and decodes the response into T. A >=400 status becomes an *Error carrying
// the status and trimmed body.
func (c *Client) do[T any](ctx context.Context, method, path string, q url.Values, req any) (*T, error) {
	var body []byte
	if req != nil {
		var err error
		if body, err = json.Marshal(req); err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
	}
	resp, err := c.send(ctx, c.http, method, path, q, body, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, ReadError(resp)
	}

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
