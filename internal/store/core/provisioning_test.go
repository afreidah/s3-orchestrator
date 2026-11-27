// -------------------------------------------------------------------------------
// Provisioning Column Helper Tests
//
// Author: Alex Freidah
//
// Both engines read and write bucket settings and grant rows through these
// helpers, so the cases here hold for either store.
// -------------------------------------------------------------------------------

package core

import (
	"reflect"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// TestEncodeBucket_EmptyIsNull verifies a bucket with no rules and no stored
// form of its own encodes both columns to nil, which both engines store as
// NULL, while an empty stored form is a set with no layer rather than none.
func TestEncodeBucket_EmptyIsNull(t *testing.T) {
	t.Parallel()

	for _, rules := range [][]config.CORSRule{nil, {}} {
		cols, err := EncodeBucket(&config.BucketSettings{CORS: rules})
		if err != nil {
			t.Fatalf("EncodeBucket(%v): %v", rules, err)
		}
		if cols.CORS != nil || cols.StoredForm != nil {
			t.Errorf("EncodeBucket(%v) = %+v, want both nil", rules, cols)
		}
	}
	cols, err := EncodeBucket(&config.BucketSettings{StoredForm: &[]string{}})
	if err != nil || cols.StoredForm == nil || *cols.StoredForm != 0 {
		t.Errorf("EncodeBucket(empty form) = (%+v, %v), want a zero form", cols, err)
	}
}

// TestBucket_RoundTrip verifies a bucket's settings read back as written, its
// stored form as the bits object_locations.form uses.
func TestBucket_RoundTrip(t *testing.T) {
	t.Parallel()

	settings := config.BucketSettings{
		MaxMultipartUploads: 5,
		CORS: []config.CORSRule{{
			AllowedOrigins: []string{"https://example.com"},
			AllowedMethods: []string{"GET", "PUT"},
			MaxAge:         600,
		}},
		StoredForm:        &[]string{config.LayerCompressed, config.LayerHashed},
		ReplicationFactor: 2,
	}
	cols, err := EncodeBucket(&settings)
	if err != nil {
		t.Fatalf("EncodeBucket: %v", err)
	}
	if got := StoredFlags(*cols.StoredForm); got != StoredCompressed|StoredHashed {
		t.Errorf("stored form column = %v, want compressed,hashed", got)
	}
	got, err := DecodeBucket(&cols)
	if err != nil {
		t.Fatalf("DecodeBucket: %v", err)
	}
	if !reflect.DeepEqual(got, settings) {
		t.Errorf("round trip = %+v, want %+v", got, settings)
	}
}

// TestBucketColumns_RejectGarbage verifies a CORS column holding something that
// is not a rule set is an error rather than a bucket with no CORS, that empty
// columns read as no rules and the deployment's form, and that a stored form
// naming something other than a layer is refused before it is written.
func TestBucketColumns_RejectGarbage(t *testing.T) {
	t.Parallel()

	if _, err := DecodeBucket(&BucketColumns{CORS: []byte("{not json")}); err == nil {
		t.Error("DecodeBucket accepted a value that is not a rule set")
	}
	got, err := DecodeBucket(&BucketColumns{})
	if err != nil || got.CORS != nil || got.StoredForm != nil {
		t.Errorf("DecodeBucket(NULL) = (%+v, %v), want no rules and no form", got, err)
	}
	if _, err := EncodeBucket(&config.BucketSettings{StoredForm: &[]string{"gzipped"}}); err == nil {
		t.Error("EncodeBucket accepted a layer nothing names")
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
