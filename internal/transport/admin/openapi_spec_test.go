// -------------------------------------------------------------------------------
// Admin API - Generated OpenAPI Description
//
// Author: Alex Freidah
//
// docs/openapi.yaml is generated from the route table, not written by hand.
// This test regenerates it and fails when the committed copy is stale, so a
// changed response shape or a new endpoint cannot ship with the description
// left behind. Run with -update (or `make openapi`) to rewrite the file.
//
// Living in a test file is deliberate: the reflection-based generator is
// imported from here alone, so no documentation tooling is linked into the
// server binary.
// -------------------------------------------------------------------------------

package admin

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/openapi"
)

// updateSpec rewrites the committed description instead of asserting against
// it. Wired to `make openapi`.
var updateSpec = flag.Bool("update", false, "rewrite docs/openapi.yaml from the route table")

// errGenerate is the failure message shared by the tests that generate.
const errGenerate = "generate: %v"

// specPath is the committed description, relative to this package.
const specPath = "../../../docs/openapi.yaml"

// specInfo identifies the generated document. Version is the API surface's
// version, deliberately not the build version: it changes when the contract
// changes, not on every release.
var specInfo = openapi.Info{
	Title:   "s3-orchestrator Admin API",
	Version: "1.0.0",
	Description: "Operational control plane for a running s3-orchestrator instance. " +
		"Every endpoint requires the admin token; long-running operations can stream " +
		"newline-delimited progress events when the caller asks for them.",
}

// specSecurity describes the token every route is mounted behind.
var specSecurity = openapi.SecurityScheme{
	Name:        "adminToken",
	HeaderName:  "X-Admin-Token",
	Description: "Shared admin token, configured as server.admin_token",
}

// openAPIRoutes projects the route table onto the generator's descriptors. The
// projection is intentionally mechanical: everything the description needs is
// already declared on the table entry.
func openAPIRoutes(rts []route) []openapi.Route {
	out := make([]openapi.Route, 0, len(rts))
	for i := range rts {
		rt := &rts[i]
		params := make([]openapi.Param, 0, len(rt.Params))
		for _, p := range rt.Params {
			params = append(params, openapi.Param{
				Name:        p.Name,
				In:          p.In,
				Description: p.Description,
				Required:    p.Required,
				Type:        p.Type,
			})
		}
		r := openapi.Route{
			Method:       rt.Method,
			Path:         rt.Pattern,
			Summary:      rt.Summary,
			Params:       params,
			Request:      rt.Request,
			Response:     rt.Response,
			Stream:       rt.Stream,
			Alt:          rt.Alt,
			ResponseType: rt.ResponseType,
			RequestType:  rt.RequestType,
		}
		if rt.Stream != nil {
			r.StreamMediaType = adminstream.ContentType
		}
		out = append(out, r)
	}
	return out
}

// TestOpenAPISpec_MatchesRouteTable regenerates the description and compares it
// to the committed file. A failure means the two have diverged: either
// regenerate, or the change was not intended.
func TestOpenAPISpec_MatchesRouteTable(t *testing.T) {
	t.Parallel()
	got, err := openapi.Generate(specInfo, specSecurity, openAPIRoutes(newTestHandler(t).routes()))
	if err != nil {
		t.Fatalf(errGenerate, err)
	}

	if *updateSpec {
		if err := os.WriteFile(filepath.Clean(specPath), got, 0o600); err != nil {
			t.Fatalf("write %s: %v", specPath, err)
		}
		t.Logf("wrote %s (%d bytes)", specPath, len(got))
		return
	}

	want, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatalf("read %s (run `make openapi` to create it): %v", specPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is stale: regenerate with `make openapi`", specPath)
	}
}

// TestOpenAPISpec_IsDeterministic guards the property the staleness check
// depends on: generating twice from the same table must produce identical
// bytes, or CI would fail on unrelated changes.
func TestOpenAPISpec_IsDeterministic(t *testing.T) {
	t.Parallel()
	routes := openAPIRoutes(newTestHandler(t).routes())
	first, err := openapi.Generate(specInfo, specSecurity, routes)
	if err != nil {
		t.Fatalf(errGenerate, err)
	}
	for range 3 {
		again, err := openapi.Generate(specInfo, specSecurity, routes)
		if err != nil {
			t.Fatalf(errGenerate, err)
		}
		if !bytes.Equal(again, first) {
			t.Fatal("generation is not deterministic across runs")
		}
	}
}
