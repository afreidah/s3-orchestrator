// -------------------------------------------------------------------------------
// Admin API - Grant Addressing Tests
//
// Author: Alex Freidah
//
// The set and delete routes address a grant identically, so the resolution they
// share is covered once here rather than twice through the handlers. What
// matters is that a caller writing the path one way and the query another still
// names the grant the store keyed.
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestGrantTarget covers every shape a grant route is addressed in: the bucket
// default, each named kind, the wildcard, and the retired spelling.
func TestGrantTarget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		query    string
		pathName string
		want     core.Resource
	}{
		{
			name:     "no kind is the bucket",
			pathName: "photos",
			want:     core.Resource{Kind: core.ResourceBucket, Name: "photos"},
		},
		{
			name:     "an explicit bucket",
			query:    "?kind=bucket",
			pathName: "photos",
			want:     core.Resource{Kind: core.ResourceBucket, Name: "photos"},
		},
		{
			name:     "one named backend",
			query:    "?kind=backend",
			pathName: "wasabi-eu",
			want:     core.Resource{Kind: core.ResourceBackend, Name: "wasabi-eu"},
		},
		{
			name:     "every backend",
			query:    "?kind=backend",
			pathName: "*",
			want:     core.Resource{Kind: core.ResourceBackend, Name: core.ResourceWildcard},
		},
		{
			// The path segment is a placeholder a caller has to send something
			// for; the kind is what says it means nothing.
			name:     "the orchestrator discards its placeholder",
			query:    "?kind=orchestrator",
			pathName: "orchestrator",
			want:     core.Resource{Kind: core.ResourceOrchestrator},
		},
		{
			name:     "the retired instance spelling",
			query:    "?kind=instance",
			pathName: "instance",
			want:     core.Resource{Kind: core.ResourceOrchestrator},
		},
		{
			// Left alone rather than corrected, so the refusal names what was
			// asked for rather than something the server substituted.
			name:     "an unknown kind is passed through to be refused",
			query:    "?kind=nonsense",
			pathName: "photos",
			want:     core.Resource{Kind: core.ResourceKind("nonsense"), Name: "photos"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete,
				"/admin/api/provisioning/grants/u1/"+tc.pathName+tc.query, nil)
			req.SetPathValue(paramID, "u1")
			req.SetPathValue(paramName, tc.pathName)

			user, got := grantTarget(req)
			if user != "u1" {
				t.Errorf("user = %q, want u1", user)
			}
			if got != tc.want {
				t.Errorf("resource = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestGrantTarget_AddressesTheSameGrantForBothRoutes pins what the shared
// resolution exists for: a set and a delete written the same way have to reach
// the same row, or declaring access and withdrawing it would disagree about
// which grant they meant.
func TestGrantTarget_AddressesTheSameGrantForBothRoutes(t *testing.T) {
	t.Parallel()

	const target = "/admin/api/provisioning/grants/u1/wasabi-eu?kind=backend"
	put := httptest.NewRequestWithContext(t.Context(), http.MethodPut, target, nil)
	del := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, target, nil)
	for _, r := range []*http.Request{put, del} {
		r.SetPathValue(paramID, "u1")
		r.SetPathValue(paramName, "wasabi-eu")
	}

	setUser, setResource := grantTarget(put)
	delUser, delResource := grantTarget(del)
	if setUser != delUser || setResource != delResource {
		t.Errorf("set addressed %s/%+v, delete addressed %s/%+v; want the same grant",
			setUser, setResource, delUser, delResource)
	}
}
