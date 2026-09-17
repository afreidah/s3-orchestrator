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

// -------------------------------------------------------------------------
// PERMISSION SHORTHANDS
// -------------------------------------------------------------------------

// The orchestrator expands all and admin-all when it stores a grant and returns
// the expansion when it reads one back. A state holding the shorthand is left
// alone only when it still stands for exactly what is stored, so the cases that
// matter are the near misses: a set that is one permission short, one that has
// been widened out of band, and a shorthand that is not one.
func TestStandsFor(t *testing.T) {
	t.Parallel()

	everyBucketPermission := []string{"list-buckets", "list", "read", "write", "delete", "tags"}

	cases := []struct {
		name   string
		held   []string
		stored []string
		want   bool
	}{
		{
			name:   "all stands for every bucket permission",
			held:   []string{"all"},
			stored: everyBucketPermission,
			want:   true,
		},
		{
			name:   "order does not matter, because the orchestrator holds a set",
			held:   []string{"all"},
			stored: []string{"tags", "delete", "write", "read", "list", "list-buckets"},
			want:   true,
		},
		{
			name: "admin-all stands for every administrative permission",
			held: []string{"admin-all"},
			stored: []string{
				"admin-read", "admin-logs", "admin-maintain", "admin-convert", "admin-keys",
				"admin-cache", "admin-drain", "admin-decommission", "admin-config",
				"admin-provision",
			},
			want: true,
		},
		{
			name:   "a narrowed grant is a real change, not a shorthand",
			held:   []string{"all"},
			stored: []string{"list", "read", "write", "delete", "tags"},
			want:   false,
		},
		{
			name:   "same size but a different permission",
			held:   []string{"all"},
			stored: []string{"list-buckets", "list", "read", "write", "delete", "admin-read"},
			want:   false,
		},
		{
			name:   "the wrong vocabulary's expansion",
			held:   []string{"admin-all"},
			stored: everyBucketPermission,
			want:   false,
		},
		{
			name:   "an explicit set is compared as written",
			held:   []string{"list", "read"},
			stored: []string{"list", "read"},
			want:   false,
		},
		{
			name:   "a lone permission that is not a shorthand",
			held:   []string{"read"},
			stored: []string{"read"},
			want:   false,
		},
		{
			name:   "nothing held, as on import",
			held:   nil,
			stored: everyBucketPermission,
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := standsFor(tc.held, tc.stored); got != tc.want {
				t.Errorf("standsFor(%v, %v) = %v, want %v", tc.held, tc.stored, got, tc.want)
			}
		})
	}
}
