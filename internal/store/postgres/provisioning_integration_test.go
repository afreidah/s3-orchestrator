// -------------------------------------------------------------------------------
// Provisioning Integration Tests
//
// Author: Alex Freidah
//
// Round-trips each provisioning table against a real database and pins the two
// rules the schema enforces rather than the code: a user still holding
// credentials or grants cannot be removed, and removing one credential or grant
// leaves its siblings.
//
// The SQLite suite covers the same contract, so a divergence between the engines
// fails one of the two rather than going unnoticed.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// seedProvisioningUser creates a user scoped to the calling test and removes it
// afterwards, so the shared fixture database carries nothing between tests.
func seedProvisioningUser(t *testing.T, s *Store, id string) string {
	t.Helper()
	ctx := context.Background()
	scoped := uniqueKey(t, id)
	if err := s.CreateUser(ctx, &core.User{ID: scoped, Name: scoped}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteUser(context.Background(), scoped) })
	return scoped
}

// findProvisionedBucket returns the bucket with the given name from a listing.
func findProvisionedBucket(buckets []core.Bucket, name string) (core.Bucket, bool) {
	for _, b := range buckets {
		if b.Name == name {
			return b, true
		}
	}
	return core.Bucket{}, false
}

// grantsFor returns the grants belonging to one user.
func grantsFor(grants []core.Grant, userID string) []core.Grant {
	out := make([]core.Grant, 0, len(grants))
	for _, g := range grants {
		if g.UserID == userID {
			out = append(out, g)
		}
	}
	return out
}

// credentialsFor returns the credentials belonging to one user.
func credentialsFor(creds []core.Credential, userID string) []core.Credential {
	out := make([]core.Credential, 0, len(creds))
	for _, c := range creds {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	return out
}

// -------------------------------------------------------------------------
// ROUND TRIPS
// -------------------------------------------------------------------------

// TestProvisioningInt_BucketRoundTrip verifies a bucket survives a write and
// read with its settings and CORS rules intact.
func TestProvisioningInt_BucketRoundTrip(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	name := uniqueKey(t, "bucket")

	want := core.Bucket{
		Name:                name,
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
	t.Cleanup(func() { _ = s.DeleteBucket(context.Background(), name) })

	all, err := s.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	got, ok := findProvisionedBucket(all, name)
	if !ok {
		t.Fatalf("bucket %q missing from the listing", name)
	}
	if got.MaxMultipartUploads != 12 {
		t.Errorf("limit = %d, want 12", got.MaxMultipartUploads)
	}
	if len(got.CORS) != 1 || got.CORS[0].MaxAge != 600 {
		t.Errorf("cors = %+v, want the rule written", got.CORS)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at is zero, want the write time")
	}
}

// TestProvisioningInt_BucketWithoutCORS verifies a bucket carrying no rules
// reads back with none rather than an empty set.
func TestProvisioningInt_BucketWithoutCORS(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	name := uniqueKey(t, "plain")

	if err := s.CreateBucket(ctx, &core.Bucket{Name: name}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteBucket(context.Background(), name) })

	all, err := s.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	got, ok := findProvisionedBucket(all, name)
	if !ok {
		t.Fatalf("bucket %q missing from the listing", name)
	}
	if got.CORS != nil {
		t.Errorf("cors = %+v, want nil", got.CORS)
	}
}

// TestProvisioningInt_CredentialRoundTrip verifies a keypair survives with its
// user, label and disabled flag, and that last_used_at reads back absent until
// something sets it.
func TestProvisioningInt_CredentialRoundTrip(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	userID := seedProvisioningUser(t, s, "user")
	accessKey := uniqueKey(t, "AK")

	if err := s.CreateCredential(ctx, &core.Credential{
		AccessKeyID: accessKey,
		UserID:      userID,
		Secret:      "SK",
		Label:       "ci pipeline",
		Disabled:    true,
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteCredential(context.Background(), accessKey) })

	all, err := s.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	mine := credentialsFor(all, userID)
	if len(mine) != 1 {
		t.Fatalf("credentials for the user = %d, want 1", len(mine))
	}
	c := mine[0]
	if c.AccessKeyID != accessKey || c.Secret != "SK" {
		t.Errorf("credential = %+v, want %s/SK", c, accessKey)
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

// TestProvisioningInt_CredentialWithoutLabel verifies an unlabelled credential
// reads back with an empty label rather than failing on a NULL column.
func TestProvisioningInt_CredentialWithoutLabel(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	userID := seedProvisioningUser(t, s, "user")
	accessKey := uniqueKey(t, "AK")

	if err := s.CreateCredential(ctx, &core.Credential{
		AccessKeyID: accessKey, UserID: userID, Secret: "SK",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteCredential(context.Background(), accessKey) })

	all, err := s.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	mine := credentialsFor(all, userID)
	if len(mine) != 1 || mine[0].Label != "" {
		t.Errorf("label = %q, want empty", mine[0].Label)
	}
}

// TestProvisioningInt_GrantRoundTrip verifies a grant survives, including one
// naming a bucket the store does not hold, which is legal because config can
// declare the bucket instead.
func TestProvisioningInt_GrantRoundTrip(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	userID := seedProvisioningUser(t, s, "user")

	if err := s.CreateGrant(ctx, &core.Grant{UserID: userID, BucketName: "declared-in-config"}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteGrant(context.Background(), userID, "declared-in-config") })

	all, err := s.ListGrants(ctx)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	mine := grantsFor(all, userID)
	if len(mine) != 1 || mine[0].BucketName != "declared-in-config" {
		t.Fatalf("grants = %+v, want one naming declared-in-config", mine)
	}
}

// -------------------------------------------------------------------------
// SCHEMA RULES
// -------------------------------------------------------------------------

// TestProvisioningInt_DeleteUserRefusedWhileCredentialsExist verifies the
// foreign key holds, so removing a user is never a silent revocation of its
// keypairs.
func TestProvisioningInt_DeleteUserRefusedWhileCredentialsExist(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	userID := seedProvisioningUser(t, s, "user")
	accessKey := uniqueKey(t, "AK")

	if err := s.CreateCredential(ctx, &core.Credential{
		AccessKeyID: accessKey, UserID: userID, Secret: "SK",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if err := s.DeleteUser(ctx, userID); err == nil {
		t.Fatal("DeleteUser succeeded while a credential still referenced the user")
	}

	if err := s.DeleteCredential(ctx, accessKey); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if err := s.DeleteUser(ctx, userID); err != nil {
		t.Fatalf("DeleteUser after removing the credential: %v", err)
	}
}

// TestProvisioningInt_DeleteUserRefusedWhileGrantsExist verifies the same holds
// for grants.
func TestProvisioningInt_DeleteUserRefusedWhileGrantsExist(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	userID := seedProvisioningUser(t, s, "user")

	if err := s.CreateGrant(ctx, &core.Grant{UserID: userID, BucketName: "photos"}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteGrant(context.Background(), userID, "photos") })

	if err := s.DeleteUser(ctx, userID); err == nil {
		t.Fatal("DeleteUser succeeded while a grant still referenced the user")
	}
}

// TestProvisioningInt_DeleteGrantLeavesSiblings verifies withdrawing one bucket
// leaves a user's other grants in place, which is what makes a grant
// individually addressable.
func TestProvisioningInt_DeleteGrantLeavesSiblings(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	userID := seedProvisioningUser(t, s, "user")

	for _, b := range []string{"photos", "backups"} {
		if err := s.CreateGrant(ctx, &core.Grant{UserID: userID, BucketName: b}); err != nil {
			t.Fatalf("CreateGrant(%s): %v", b, err)
		}
	}
	t.Cleanup(func() { _ = s.DeleteGrant(context.Background(), userID, "backups") })

	if err := s.DeleteGrant(ctx, userID, "photos"); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	all, err := s.ListGrants(ctx)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	mine := grantsFor(all, userID)
	if len(mine) != 1 || mine[0].BucketName != "backups" {
		t.Errorf("grants = %+v, want only backups", mine)
	}
}

// TestProvisioningInt_DeleteCredentialLeavesSiblings verifies revoking one
// keypair leaves a user's others working, which is the point of a user holding
// several.
func TestProvisioningInt_DeleteCredentialLeavesSiblings(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	userID := seedProvisioningUser(t, s, "user")
	first, second := uniqueKey(t, "AK1"), uniqueKey(t, "AK2")

	for _, ak := range []string{first, second} {
		if err := s.CreateCredential(ctx, &core.Credential{
			AccessKeyID: ak, UserID: userID, Secret: "SK",
		}); err != nil {
			t.Fatalf("CreateCredential(%s): %v", ak, err)
		}
	}
	t.Cleanup(func() { _ = s.DeleteCredential(context.Background(), second) })

	if err := s.DeleteCredential(ctx, first); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	all, err := s.ListCredentials(ctx)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	mine := credentialsFor(all, userID)
	if len(mine) != 1 || mine[0].AccessKeyID != second {
		t.Errorf("credentials = %+v, want only %s", mine, second)
	}
}
