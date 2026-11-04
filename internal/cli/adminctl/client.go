// -------------------------------------------------------------------------------
// Admin CLI - HTTP Client and Response Rendering
//
// Author: Alex Freidah
//
// The transport seam shared by every admin subcommand. A client carries the
// signed API client for the resolved target, the output format, and writers;
// its verb methods issue one authenticated request and render the outcome. JSON
// mode pretty-prints the server's raw bytes (byte-stable for scripts); text
// mode runs a per-command renderer, falling back to a generic key/value view.
// -------------------------------------------------------------------------------

package adminctl

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/cli/flagset"

	"github.com/afreidah/s3-orchestrator/internal/cli/output"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// client carries the shared admin transport, output format, and writers used
// by every admin subcommand handler. helped records a command that stopped to
// print its flags.
type client struct {
	api    *adminclient.Client
	format output.Format
	stdout io.Writer
	stderr io.Writer
	helped bool
}

// renderFunc renders a successful response body in text mode. A nil renderer
// falls back to the generic key/value renderer.
type renderFunc func(w io.Writer, body []byte) error

// typed builds a renderer that decodes the body into the wire type T before
// render writes it. A body that does not decode falls back to raw JSON, as any
// failed render does.
func typed[T any](render func(io.Writer, *T) error) renderFunc {
	return func(w io.Writer, body []byte) error {
		var v T
		if err := json.Unmarshal(body, &v); err != nil {
			return err
		}
		return render(w, &v)
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// get/post/delete issue the named verb against path (appended to the resolved
// base address) and render the response per the client's format. A request
// with a JSON body goes through sendJSON.
func (c *client) get(path string, render renderFunc) int {
	return c.do(http.MethodGet, path, nil, render)
}

func (c *client) post(path string, render renderFunc) int {
	return c.do(http.MethodPost, path, nil, render)
}

func (c *client) delete(path string) int {
	return c.do(http.MethodDelete, path, nil, nil)
}

// sendJSON encodes req as the request body and issues method against path,
// rendering the response like do. An unencodable request is reported to
// stderr and returns exit code 1.
func (c *client) sendJSON(method, path string, req any, render renderFunc) int {
	body, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(c.stderr, "error: encode request: %v\n", err)
		return 1
	}
	return c.do(method, path, body, render)
}

// do performs one admin request and renders the outcome per format, returning
// the exit code: 1 for a failed call, which reportError has rendered.
func (c *client) do(method, path string, body []byte, render renderFunc) int {
	data, ok := c.request(method, path, body)
	if !ok {
		return 1
	}
	c.renderSuccess(data, render)
	return 0
}

// request issues an authenticated request and returns the response body,
// reporting a failed call and returning false.
func (c *client) request(method, path string, body []byte) ([]byte, bool) {
	data, err := c.api.Bytes(context.Background(), method, path, nil, body)
	if err != nil {
		c.reportError(err)
		return nil, false
	}
	return data, true
}

// flags starts a subcommand's flag set, reporting parse errors on stderr.
func (c *client) flags(name string) *flag.FlagSet {
	return flagset.New(name, c.stderr)
}

// parse reads args into fs, enforcing the required flags, and reports whether
// the command may go on. Commands stop with exit 1 when it may not, so a help
// request is remembered for dispatch to turn back into a success.
func (c *client) parse(fs *flag.FlagSet, args []string, required ...string) bool {
	exit, ok := flagset.Parse(fs, args, required...)
	c.helped = !ok && exit == 0
	return ok
}

// setIf sets key when value is set, so an unset flag sends nothing.
func setIf(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}

// setPositive sets key when n is above zero, so an unset count sends nothing.
func setPositive(q url.Values, key string, n int) {
	if n > 0 {
		q.Set(key, strconv.Itoa(n))
	}
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

// reportError renders a failed call. JSON mode pretty-prints an API error's
// raw body; text mode prints its detail, or any other failure, as one
// "error: <message>" line on stderr.
func (c *client) reportError(err error) {
	apiErr, ok := errors.AsType[*adminclient.Error](err)
	if !ok {
		fmt.Fprintf(c.stderr, fmtError, err)
		return
	}
	if c.format.IsJSON() {
		_ = output.PrettyJSON(c.stdout, []byte(apiErr.Body))
		return
	}
	fmt.Fprintf(c.stderr, fmtError, apiErr.Detail())
}
