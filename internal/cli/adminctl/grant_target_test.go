// -------------------------------------------------------------------------------
// Admin CLI - Grant Target Resolution Tests
//
// Author: Alex Freidah
//
// The three grant verbs name their target the same way, so the flags they share
// and the path they build are covered once here rather than three times through
// the verbs. What matters is that the alias flag, the retired kind spelling and
// the nameless orchestrator all resolve to the grant the server keys.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// resolveWith runs the shared flag parsing over args and reports what it made
// of them, along with anything written to stderr.
func resolveWith(t *testing.T, args []string) (string, core.Resource, bool, string) {
	t.Helper()
	var stderr bytes.Buffer
	c := &client{stderr: &stderr}
	fs, v := grantFlags("grant test", c)
	user, resource, ok := resolveGrant(fs, args, c, v)
	return user, resource, ok, stderr.String()
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestResolveGrant covers every way a verb is asked to name its target.
func TestResolveGrant(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want core.Resource
	}{
		{
			name: "a bucket by name",
			args: []string{"-user", "u1", "-name", "photos"},
			want: core.Resource{Kind: core.ResourceBucket, Name: "photos"},
		},
		{
			// -bucket predates grants reaching past buckets and stays an alias,
			// so an operator's existing scripts keep working.
			name: "a bucket by the older -bucket alias",
			args: []string{"-user", "u1", "-bucket", "photos"},
			want: core.Resource{Kind: core.ResourceBucket, Name: "photos"},
		},
		{
			name: "-name wins over -bucket when both are given",
			args: []string{"-user", "u1", "-name", "photos", "-bucket", "ignored"},
			want: core.Resource{Kind: core.ResourceBucket, Name: "photos"},
		},
		{
			name: "one named backend",
			args: []string{"-user", "u1", "-kind", "backend", "-name", "wasabi-eu"},
			want: core.Resource{Kind: core.ResourceBackend, Name: "wasabi-eu"},
		},
		{
			name: "every backend",
			args: []string{"-user", "u1", "-kind", "backend", "-name", "*"},
			want: core.Resource{Kind: core.ResourceBackend, Name: core.ResourceWildcard},
		},
		{
			name: "the orchestrator, which takes no name",
			args: []string{"-user", "u1", "-kind", "orchestrator"},
			want: core.Resource{Kind: core.ResourceOrchestrator},
		},
		{
			name: "the retired instance spelling",
			args: []string{"-user", "u1", "-kind", "instance"},
			want: core.Resource{Kind: core.ResourceOrchestrator},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			user, got, ok, stderr := resolveWith(t, tc.args)
			if !ok {
				t.Fatalf("resolveGrant refused valid arguments: %s", stderr)
			}
			if user != "u1" {
				t.Errorf("user = %q, want u1", user)
			}
			if got != tc.want {
				t.Errorf("resource = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestResolveGrant_Refusals verifies each refusal says which flag is missing
// rather than failing silently, since a verb that resolved nothing would go on
// to address a grant nobody named.
func TestResolveGrant_Refusals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		args     []string
		wantSaid string
	}{
		{"no user", []string{"-name", "photos"}, "-user"},
		{"a bucket with no name", []string{"-user", "u1"}, "-name"},
		{"a backend with no name", []string{"-user", "u1", "-kind", "backend"}, "-name"},
		{"an unparseable flag", []string{"-nope"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, ok, stderr := resolveWith(t, tc.args)
			if ok {
				t.Fatal("resolveGrant accepted arguments naming no grant")
			}
			if tc.wantSaid != "" && !strings.Contains(stderr, tc.wantSaid) {
				t.Errorf("stderr = %q, want it to name %s", stderr, tc.wantSaid)
			}
		})
	}
}

// TestGrantPath covers the path the set and delete verbs address a grant with.
//
// The orchestrator is the case worth pinning: it carries no name, so the path
// needs a placeholder segment the server discards, and the kind in the query is
// what tells it to.
func TestGrantPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		user     string
		resource core.Resource
		want     string
	}{
		{
			name:     "a bucket",
			user:     "u1",
			resource: core.Resource{Kind: core.ResourceBucket, Name: "photos"},
			want:     "/admin/api/provisioning/grants/u1/photos?kind=bucket",
		},
		{
			name:     "every backend, escaped",
			user:     "u1",
			resource: core.Resource{Kind: core.ResourceBackend, Name: "*"},
			want:     "/admin/api/provisioning/grants/u1/%2A?kind=backend",
		},
		{
			name:     "the orchestrator carries a placeholder segment",
			user:     "u1",
			resource: core.Resource{Kind: core.ResourceOrchestrator},
			want:     "/admin/api/provisioning/grants/u1/orchestrator?kind=orchestrator",
		},
		{
			name:     "a user id needing escaping",
			user:     "user/with slash",
			resource: core.Resource{Kind: core.ResourceBucket, Name: "photos"},
			want:     "/admin/api/provisioning/grants/user%2Fwith%20slash/photos?kind=bucket",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := grantPath(tc.user, tc.resource); got != tc.want {
				t.Errorf("grantPath = %q, want %q", got, tc.want)
			}
		})
	}
}
