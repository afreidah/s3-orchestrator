// -------------------------------------------------------------------------------
// Provisioning Column Helper Tests
//
// Author: Alex Freidah
//
// Both engines read and write bucket CORS rules and grant rows through these
// helpers, so the cases here hold for either store.
// -------------------------------------------------------------------------------

package core

import (
	"reflect"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// TestEncodeCORS_EmptyIsNull verifies a bucket with no rules encodes to nil,
// which both engines store as NULL rather than an empty array.
func TestEncodeCORS_EmptyIsNull(t *testing.T) {
	t.Parallel()

	for _, rules := range [][]config.CORSRule{nil, {}} {
		got, err := EncodeCORS(rules)
		if err != nil {
			t.Fatalf("EncodeCORS(%v): %v", rules, err)
		}
		if got != nil {
			t.Errorf("EncodeCORS(%v) = %q, want nil", rules, got)
		}
	}
}

// TestCORS_RoundTrip verifies a rule set reads back as written.
func TestCORS_RoundTrip(t *testing.T) {
	t.Parallel()

	rules := []config.CORSRule{{
		AllowedOrigins: []string{"https://example.com"},
		AllowedMethods: []string{"GET", "PUT"},
		MaxAge:         600,
	}}
	encoded, err := EncodeCORS(rules)
	if err != nil {
		t.Fatalf("EncodeCORS: %v", err)
	}
	got, err := DecodeCORS(encoded)
	if err != nil {
		t.Fatalf("DecodeCORS: %v", err)
	}
	if !reflect.DeepEqual(got, rules) {
		t.Errorf("round trip = %+v, want %+v", got, rules)
	}
}

// TestDecodeCORS_RejectsGarbage verifies a column holding something that is not
// a rule set is an error rather than a bucket with no CORS, and that an empty
// column reads as no rules.
func TestDecodeCORS_RejectsGarbage(t *testing.T) {
	t.Parallel()

	if _, err := DecodeCORS([]byte("{not json")); err == nil {
		t.Error("DecodeCORS accepted a value that is not a rule set")
	}
	got, err := DecodeCORS(nil)
	if err != nil || got != nil {
		t.Errorf("DecodeCORS(nil) = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestGrantFromColumns_ParsesKindAndPermissions verifies the stored kind
// spelling and permission list compile into the grant the request path tests.
func TestGrantFromColumns_ParsesKindAndPermissions(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	g, err := GrantFromColumns("u1", "instance", "", "admin-read", created)
	if err != nil {
		t.Fatalf("GrantFromColumns: %v", err)
	}
	want := Grant{
		UserID:      "u1",
		Resource:    Resource{Kind: ResourceOrchestrator},
		Permissions: PermAdminRead,
		CreatedAt:   created,
	}
	if g != want {
		t.Errorf("grant = %+v, want %+v", g, want)
	}
}

// TestGrantFromColumns_EmptyDependsOnKind verifies a row recording no
// permissions carries every data-plane permission on a bucket, since such rows
// predate permissions, and nothing on any other kind.
func TestGrantFromColumns_EmptyDependsOnKind(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		kind, perms string
		want        PermissionSet
	}{
		{"bucket", "", PermAll},
		{"bucket", " , ", PermAll},
		{"backend", "", 0},
		{"instance", "", 0},
	} {
		g, err := GrantFromColumns("u1", tc.kind, "x", tc.perms, time.Now())
		if err != nil {
			t.Fatalf("GrantFromColumns(%s, %q): %v", tc.kind, tc.perms, err)
		}
		if g.Permissions != tc.want {
			t.Errorf("GrantFromColumns(%s, %q) = %q, want %q", tc.kind, tc.perms, g.Permissions, tc.want)
		}
	}
}

// TestGrantFromColumns_RejectsUnknownPermission verifies a stored value nothing
// recognises fails the read rather than resolving to some set.
func TestGrantFromColumns_RejectsUnknownPermission(t *testing.T) {
	t.Parallel()

	if _, err := GrantFromColumns("u1", "bucket", "photos", "readwrite", time.Now()); err == nil {
		t.Error("GrantFromColumns accepted an unknown permission")
	}
}
