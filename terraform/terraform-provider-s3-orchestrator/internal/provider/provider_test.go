// -------------------------------------------------------------------------------
// Provider - Unit Tests
//
// Author: Alex Freidah
//
// The branches an acceptance test cannot reach: a misconfigured provider, a
// resource handed the wrong data by the framework, and an import identifier
// that does not parse. None of these involve a running orchestrator, so they
// run without Docker and without TF_ACC.
// -------------------------------------------------------------------------------

package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// -------------------------------------------------------------------------
// CONFIGURATION
// -------------------------------------------------------------------------

// TestValueOr covers the fallback every provider attribute has. A configured
// value wins; a null one falls through to the environment.
func TestValueOr(t *testing.T) {
	const env = "S3O_TEST_VALUE_OR"
	cases := []struct {
		name       string
		configured types.String
		exported   string
		want       string
	}{
		{"a configured value wins", types.StringValue("configured"), "exported", "configured"},
		{"a null value falls through", types.StringNull(), "exported", "exported"},
		{"neither leaves it empty", types.StringNull(), "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(env, tc.exported)
			if got := valueOr(tc.configured, env); got != tc.want {
				t.Errorf("valueOr = %q, want %q", got, tc.want)
			}
		})
	}
}

// -------------------------------------------------------------------------
// RESOURCE WIRING
// -------------------------------------------------------------------------

// TestConfigureIgnoresAbsentProviderData covers the framework calling Configure
// during validation, before the provider has been configured. No data is an
// ordinary state there rather than a fault.
func TestConfigureIgnoresAbsentProviderData(t *testing.T) {
	t.Parallel()
	for name, r := range configurableResources() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var resp resource.ConfigureResponse
			configure(t, r, resource.ConfigureRequest{}, &resp)
			if resp.Diagnostics.HasError() {
				t.Errorf("diagnostics = %v, want none", resp.Diagnostics)
			}
		})
	}
}

// TestConfigureRejectsUnexpectedProviderData covers the type assertion. It can
// only fail through a provider bug, and saying so beats a nil dereference on
// the first apply.
func TestConfigureRejectsUnexpectedProviderData(t *testing.T) {
	t.Parallel()
	for name, r := range configurableResources() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var resp resource.ConfigureResponse
			configure(t, r, resource.ConfigureRequest{ProviderData: "not a client"}, &resp)
			if !resp.Diagnostics.HasError() {
				t.Fatal("diagnostics = none, want an error")
			}
			if summary := resp.Diagnostics.Errors()[0].Summary(); summary != "Unexpected provider data" {
				t.Errorf("summary = %q, want %q", summary, "Unexpected provider data")
			}
		})
	}
}

// -------------------------------------------------------------------------
// IMPORT
// -------------------------------------------------------------------------

// TestGrantImportRejectsMalformedIdentifier covers the parser. A grant has no
// id of its own, so the identifier is three parts joined, and one that does not
// come apart has to say what was expected rather than half-populate state.
func TestGrantImportRejectsMalformedIdentifier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		id   string
	}{
		{"too few segments", "user-abc/bucket"},
		{"too many segments", "user-abc/bucket/photos/extra"},
		{"no user", "/bucket/photos"},
		{"no kind", "user-abc//photos"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &grantResource{}
			var resp resource.ImportStateResponse
			r.ImportState(context.Background(), resource.ImportStateRequest{ID: tc.id}, &resp)
			if !resp.Diagnostics.HasError() {
				t.Fatalf("id %q was accepted, want a refusal", tc.id)
			}
			detail := resp.Diagnostics.Errors()[0].Detail()
			if !strings.Contains(detail, "user_id/kind/name") {
				t.Errorf("detail = %q, want it to name the expected shape", detail)
			}
		})
	}
}

// -------------------------------------------------------------------------
// UNREACHABLE PATHS
// -------------------------------------------------------------------------

// TestCredentialUpdateIsUnreachable covers the method that exists only to
// satisfy the interface. Every attribute of a credential replaces it, because a
// keypair cannot be changed in place, so reaching this is a provider bug.
func TestCredentialUpdateIsUnreachable(t *testing.T) {
	t.Parallel()
	r := &credentialResource{}
	var resp resource.UpdateResponse
	r.Update(context.Background(), resource.UpdateRequest{}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("diagnostics = none, want an error")
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// TestOptionalString covers the rendering of a field the API omits when empty,
// which has to read as null rather than as an empty string no configuration
// wrote.
func TestOptionalString(t *testing.T) {
	t.Parallel()
	if got := optionalString(""); !got.IsNull() {
		t.Errorf("optionalString(\"\") = %v, want null", got)
	}
	if got := optionalString("photos"); got.ValueString() != "photos" {
		t.Errorf("optionalString(\"photos\") = %v, want photos", got)
	}
}

// configurableResources is every resource the provider registers, keyed by the
// name a subtest reports under.
func configurableResources() map[string]resource.Resource {
	return map[string]resource.Resource{
		"user":       NewUserResource(),
		"credential": NewCredentialResource(),
		"grant":      NewGrantResource(),
	}
}

// configure calls Configure on a resource that implements it, failing the test
// rather than silently skipping one that does not.
func configure(
	t *testing.T, r resource.Resource, req resource.ConfigureRequest, resp *resource.ConfigureResponse,
) {
	t.Helper()
	c, ok := r.(resource.ResourceWithConfigure)
	if !ok {
		t.Fatalf("%T does not implement ResourceWithConfigure", r)
	}
	c.Configure(context.Background(), req, resp)
}
