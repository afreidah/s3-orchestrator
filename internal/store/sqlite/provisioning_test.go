// -------------------------------------------------------------------------------
// SQLite Store - Provisioning Tests
//
// Author: Alex Freidah
//
// Round-trips each provisioning table and covers the two rules the schema
// enforces rather than the code: a user still holding credentials or grants
// cannot be removed, and one grant leaves a user's others in place.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"database/sql"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// seedUser creates a user and returns its id, for the tests that need one to
// hang a credential or a grant off.
func seedUser(t *testing.T, s *Store, id, name string) string {
	t.Helper()
	if err := s.CreateUser(context.Background(), &core.User{ID: id, Name: name}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id
}

// -------------------------------------------------------------------------
// ROUND TRIPS
// -------------------------------------------------------------------------

// TestProvisioning_BucketRoundTrip verifies a bucket survives a write and read
// with its settings and CORS rules intact.
func TestProvisioning_BucketRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)

	want := core.Bucket{
		Name:                "photos",
		MaxMultipartUploads: 12,
		CORS: []config.CORSRule{{
			AllowedOrigins: []string{"https://example.test"},
			AllowedMethods: []string{"GET"},
			ExposeHeaders:  []string{"ETag"},
			MaxAge:         600,
		}},
	}
	if err := s.CreateBucket(ctx, &want); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	got, err := s.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListBuckets returned %d rows, want 1", len(got))
	}
	if got[0].Name != want.Name || got[0].MaxMultipartUploads != want.MaxMultipartUploads {
		t.Errorf("bucket = %+v, want name %q limit %d", got[0], want.Name, want.MaxMultipartUploads)
	}
	if len(got[0].CORS) != 1 || got[0].CORS[0].MaxAge != 600 {
		t.Errorf("cors = %+v, want the rule written", got[0].CORS)
	}
	if got[0].CreatedAt.IsZero() {
		t.Error("created_at is zero, want the write time")
	}
}

// TestProvisioning_BucketWithoutCORS verifies a bucket carrying no rules reads
// back with none rather than an empty set, so "no CORS configured" is one value
// in the column rather than two.
func TestProvisioning_BucketWithoutCORS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.CreateBucket(ctx, &core.Bucket{Name: "plain"}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	got, err := s.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(got) != 1 || got[0].CORS != nil {
		t.Errorf("cors = %+v, want nil", got[0].CORS)
	}
}

// TestProvisioning_CredentialRoundTrip verifies a keypair survives with its
// user, label and disabled flag, and that last_used_at reads back absent until
// something sets it.
func TestProvisioning_CredentialRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	userID := seedUser(t, s, "u1", "ci")

	want := core.Credential{
		AccessKeyID: "AK",
		UserID:      userID,
		Secret:      "SK",
		Label:       "ci pipeline",
		Disabled:    true,
	}
	if err := s.CreateCredential(ctx, &want); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	got, err := s.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListCredentials returned %d rows, want 1", len(got))
	}
	c := got[0]
	if c.AccessKeyID != "AK" || c.UserID != userID || c.Secret != "SK" {
		t.Errorf("credential = %+v, want AK/%s/SK", c, userID)
	}
	if c.Label != "ci pipeline" {
		t.Errorf("label = %q, want %q", c.Label, "ci pipeline")
	}
	if !c.Disabled {
		t.Error("disabled = false, want true")
	}
	if c.LastUsedAt != nil {
		t.Errorf("last_used_at = %v, want nil until something sets it", c.LastUsedAt)
	}
}

// TestProvisioning_CredentialWithoutLabel verifies an unlabelled credential
// reads back with an empty label rather than failing on a NULL column.
func TestProvisioning_CredentialWithoutLabel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	seedUser(t, s, "u1", "ci")

	if err := s.CreateCredential(ctx, &core.Credential{
		AccessKeyID: "AK", UserID: "u1", Secret: "SK",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	got, err := s.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(got) != 1 || got[0].Label != "" {
		t.Errorf("label = %q, want empty", got[0].Label)
	}
}

// TestProvisioning_GrantRoundTrip verifies a grant survives, including one
// naming a bucket the store does not hold - which is legal, because config can
// declare the bucket instead.
func TestProvisioning_GrantRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	seedUser(t, s, "u1", "ci")

	if err := s.CreateGrant(ctx, &core.Grant{UserID: "u1", Resource: core.BucketResource("declared-in-config")}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	got, err := s.ListGrants(ctx)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if len(got) != 1 || got[0].UserID != "u1" || got[0].Resource.Name != "declared-in-config" {
		t.Fatalf("grants = %+v, want one grant u1 -> declared-in-config", got)
	}
}

// -------------------------------------------------------------------------
// SCHEMA RULES
// -------------------------------------------------------------------------

// TestProvisioning_DeleteUserRefusedWhileCredentialsExist verifies the foreign
// key holds, so removing a user is never a silent revocation of its keypairs.
func TestProvisioning_DeleteUserRefusedWhileCredentialsExist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	seedUser(t, s, "u1", "ci")

	if err := s.CreateCredential(ctx, &core.Credential{
		AccessKeyID: "AK", UserID: "u1", Secret: "SK",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if err := s.DeleteUser(ctx, "u1"); err == nil {
		t.Fatal("DeleteUser succeeded while a credential still referenced the user")
	}

	if err := s.DeleteCredential(ctx, "AK"); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if err := s.DeleteUser(ctx, "u1"); err != nil {
		t.Fatalf("DeleteUser after removing the credential: %v", err)
	}
}

// TestProvisioning_DeleteUserRefusedWhileGrantsExist verifies the same holds for
// grants.
func TestProvisioning_DeleteUserRefusedWhileGrantsExist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	seedUser(t, s, "u1", "ci")

	if err := s.CreateGrant(ctx, &core.Grant{UserID: "u1", Resource: core.BucketResource("photos")}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	if err := s.DeleteUser(ctx, "u1"); err == nil {
		t.Fatal("DeleteUser succeeded while a grant still referenced the user")
	}
}

// TestProvisioning_DeleteGrantLeavesSiblings verifies withdrawing one bucket
// leaves a user's other grants in place, which is what makes a grant
// individually addressable.
func TestProvisioning_DeleteGrantLeavesSiblings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	seedUser(t, s, "u1", "ci")

	for _, b := range []string{"photos", "backups"} {
		if err := s.CreateGrant(ctx, &core.Grant{UserID: "u1", Resource: core.BucketResource(b)}); err != nil {
			t.Fatalf("CreateGrant(%s): %v", b, err)
		}
	}
	if err := s.DeleteGrant(ctx, "u1", core.BucketResource("photos")); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}

	got, err := s.ListGrants(ctx)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if len(got) != 1 || got[0].Resource.Name != "backups" {
		t.Errorf("grants = %+v, want only backups", got)
	}
}

// TestProvisioning_DeleteCredentialLeavesSiblings verifies revoking one keypair
// leaves a user's others working, which is the point of a user holding several.
func TestProvisioning_DeleteCredentialLeavesSiblings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	seedUser(t, s, "u1", "ci")

	for _, ak := range []string{"AK1", "AK2"} {
		if err := s.CreateCredential(ctx, &core.Credential{
			AccessKeyID: ak, UserID: "u1", Secret: "SK",
		}); err != nil {
			t.Fatalf("CreateCredential(%s): %v", ak, err)
		}
	}
	if err := s.DeleteCredential(ctx, "AK1"); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}

	got, err := s.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(got) != 1 || got[0].AccessKeyID != "AK2" {
		t.Errorf("credentials = %+v, want only AK2", got)
	}
}

// TestProvisioning_UserRoundTrip verifies a user survives a write and read.
func TestProvisioning_UserRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	seedUser(t, s, "u1", "ci")

	got, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListUsers returned %d rows, want 1", len(got))
	}
	if got[0].ID != "u1" || got[0].Name != "ci" {
		t.Errorf("user = %+v, want u1/ci", got[0])
	}
	if got[0].CreatedAt.IsZero() {
		t.Error("created_at is zero, want the write time")
	}
}

// TestProvisioning_DeleteBucket verifies a stored bucket can be removed.
func TestProvisioning_DeleteBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.CreateBucket(ctx, &core.Bucket{Name: "photos"}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := s.DeleteBucket(ctx, "photos"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	got, err := s.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("buckets = %+v, want none", got)
	}
}

// TestProvisioning_DuplicateWritesRefused verifies each table's key holds, so a
// second write of the same identity surfaces as an error rather than silently
// replacing what is there.
func TestProvisioning_DuplicateWritesRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	seedUser(t, s, "u1", "ci")

	if err := s.CreateUser(ctx, &core.User{ID: "u1", Name: "other"}); err == nil {
		t.Error("CreateUser accepted a duplicate id")
	}
	if err := s.CreateUser(ctx, &core.User{ID: "u2", Name: "ci"}); err == nil {
		t.Error("CreateUser accepted a duplicate name")
	}

	if err := s.CreateBucket(ctx, &core.Bucket{Name: "photos"}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if err := s.CreateBucket(ctx, &core.Bucket{Name: "photos"}); err == nil {
		t.Error("CreateBucket accepted a duplicate name")
	}

	cred := core.Credential{AccessKeyID: "AK", UserID: "u1", Secret: "SK"}
	if err := s.CreateCredential(ctx, &cred); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if err := s.CreateCredential(ctx, &cred); err == nil {
		t.Error("CreateCredential accepted a duplicate access key")
	}

	grant := core.Grant{UserID: "u1", Resource: core.BucketResource("photos")}
	if err := s.CreateGrant(ctx, &grant); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	if err := s.CreateGrant(ctx, &grant); err == nil {
		t.Error("CreateGrant accepted a duplicate pair")
	}
}

// TestProvisioning_WritesRequireTheirUser verifies the foreign keys reject a
// credential or a grant whose user does not exist, so neither can be orphaned
// at the moment it is written.
func TestProvisioning_WritesRequireTheirUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.CreateCredential(ctx, &core.Credential{
		AccessKeyID: "AK", UserID: "absent", Secret: "SK",
	}); err == nil {
		t.Error("CreateCredential accepted an unknown user")
	}
	if err := s.CreateGrant(ctx, &core.Grant{UserID: "absent", Resource: core.BucketResource("photos")}); err == nil {
		t.Error("CreateGrant accepted an unknown user")
	}
}

// TestProvisioning_ClosedStoreSurfacesErrors verifies every provisioning
// statement reports a database that is gone rather than answering as though the
// tables were empty. Assembly runs at boot and on every reload, and an empty
// answer there would revoke every stored credential silently.
func TestProvisioning_ClosedStoreSurfacesErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	s.Close()

	if _, err := s.ListBuckets(ctx); err == nil {
		t.Error("ListBuckets succeeded against a closed store")
	}
	if _, err := s.ListUsers(ctx); err == nil {
		t.Error("ListUsers succeeded against a closed store")
	}
	if _, err := s.ListCredentials(ctx); err == nil {
		t.Error("ListCredentials succeeded against a closed store")
	}
	if _, err := s.ListGrants(ctx); err == nil {
		t.Error("ListGrants succeeded against a closed store")
	}
	if err := s.CreateBucket(ctx, &core.Bucket{Name: "b"}); err == nil {
		t.Error("CreateBucket succeeded against a closed store")
	}
	if err := s.CreateUser(ctx, &core.User{ID: "u", Name: "n"}); err == nil {
		t.Error("CreateUser succeeded against a closed store")
	}
	if err := s.CreateCredential(ctx, &core.Credential{AccessKeyID: "AK", UserID: "u", Secret: "s"}); err == nil {
		t.Error("CreateCredential succeeded against a closed store")
	}
	if err := s.CreateGrant(ctx, &core.Grant{UserID: "u", Resource: core.BucketResource("b")}); err == nil {
		t.Error("CreateGrant succeeded against a closed store")
	}
	if err := s.DeleteBucket(ctx, "b"); err == nil {
		t.Error("DeleteBucket succeeded against a closed store")
	}
	if err := s.DeleteUser(ctx, "u"); err == nil {
		t.Error("DeleteUser succeeded against a closed store")
	}
	if err := s.DeleteCredential(ctx, "AK"); err == nil {
		t.Error("DeleteCredential succeeded against a closed store")
	}
	if err := s.DeleteGrant(ctx, "u", core.BucketResource("b")); err == nil {
		t.Error("DeleteGrant succeeded against a closed store")
	}
}

// TestProvisioning_CORSEncodeSkipsEmpty verifies a bucket with no rules encodes
// to NULL rather than to an empty array.
func TestProvisioning_CORSEncodeSkipsEmpty(t *testing.T) {
	t.Parallel()

	got, err := marshalCORS(nil)
	if err != nil {
		t.Fatalf("marshalCORS: %v", err)
	}
	if got.Valid {
		t.Errorf("marshalCORS(nil) = %+v, want NULL", got)
	}
}

// TestProvisioning_CORSDecodeRejectsGarbage verifies a column holding something
// that is not a rule set surfaces as an error rather than as a bucket with no
// CORS, which would silently drop a browser policy.
func TestProvisioning_CORSDecodeRejectsGarbage(t *testing.T) {
	t.Parallel()

	if _, err := unmarshalCORS(sql.NullString{String: "{not json", Valid: true}); err == nil {
		t.Error("unmarshalCORS accepted a value that is not a rule set")
	}
	got, err := unmarshalCORS(sql.NullString{})
	if err != nil || got != nil {
		t.Errorf("unmarshalCORS(NULL) = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestProvisioning_ListingsAreOrdered verifies both engines hand back a stable
// sequence, so a caller comparing two assemblies compares content rather than
// ordering.
func TestProvisioning_ListingsAreOrdered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)

	for _, name := range []string{"zeta", "alpha", "mid"} {
		if err := s.CreateBucket(ctx, &core.Bucket{Name: name}); err != nil {
			t.Fatalf("CreateBucket(%s): %v", name, err)
		}
	}
	got, err := s.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("bucket order = %+v, want %v", got, want)
		}
	}
}

// TestProvisioning_GrantsOnEveryResourceKind is what the resource model is for:
// the control plane has no bucket, so a grant has to be able to name a backend
// or the fleet before an admin action set has anything to hang off.
func TestProvisioning_GrantsOnEveryResourceKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	seedUser(t, s, "u1", "ci")

	want := []core.Resource{
		core.BucketResource("declared-in-config"),
		{Kind: core.ResourceBackend, Name: "minio-a"},
		{Kind: core.ResourceFleet},
	}
	for _, r := range want {
		if err := s.CreateGrant(ctx, &core.Grant{UserID: "u1", Resource: r}); err != nil {
			t.Fatalf("CreateGrant(%s %q): %v", r.Kind, r.Name, err)
		}
	}

	got, err := s.ListGrants(ctx)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("grants = %d, want %d: %+v", len(got), len(want), got)
	}
	seen := make(map[core.Resource]bool, len(got))
	for i := range got {
		seen[got[i].Resource] = true
	}
	for _, r := range want {
		if !seen[r] {
			t.Errorf("grant on %s %q did not survive the round trip", r.Kind, r.Name)
		}
	}
}

// TestProvisioning_KindIsPartOfTheKey asserts a bucket and a backend sharing a
// name are two grants rather than one overwriting the other, which is why the
// kind is in the primary key.
func TestProvisioning_KindIsPartOfTheKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	seedUser(t, s, "u1", "ci")

	if err := s.CreateGrant(ctx, &core.Grant{UserID: "u1", Resource: core.BucketResource("shared")}); err != nil {
		t.Fatalf("CreateGrant(bucket): %v", err)
	}
	if err := s.CreateGrant(ctx, &core.Grant{
		UserID:   "u1",
		Resource: core.Resource{Kind: core.ResourceBackend, Name: "shared"},
	}); err != nil {
		t.Fatalf("CreateGrant(backend): %v", err)
	}

	got, err := s.ListGrants(ctx)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("grants = %d, want 2 - the kind is part of the key", len(got))
	}
}

// TestProvisioning_DeleteGrantIsScopedToTheKind asserts withdrawing a bucket
// grant leaves a backend grant of the same name alone.
func TestProvisioning_DeleteGrantIsScopedToTheKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	seedUser(t, s, "u1", "ci")

	backend := core.Resource{Kind: core.ResourceBackend, Name: "shared"}
	for _, r := range []core.Resource{core.BucketResource("shared"), backend} {
		if err := s.CreateGrant(ctx, &core.Grant{UserID: "u1", Resource: r}); err != nil {
			t.Fatalf("CreateGrant(%s): %v", r.Kind, err)
		}
	}

	if err := s.DeleteGrant(ctx, "u1", core.BucketResource("shared")); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}

	got, err := s.ListGrants(ctx)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if len(got) != 1 || got[0].Resource != backend {
		t.Errorf("grants = %+v, want only the backend grant", got)
	}
}
