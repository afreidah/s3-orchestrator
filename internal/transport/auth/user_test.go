// -------------------------------------------------------------------------------
// Auth - User and Store-Merge Tests
//
// Author: Alex Freidah
//
// Covers what a user answers about the buckets it reaches, and the registry's
// treatment of the two sources credentials arrive from: a stored keypair
// authenticating on its own, and one whose access key the config file also
// declares being shadowed rather than rejected.
// -------------------------------------------------------------------------------

package auth

import (
	"net/http"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// USER
// -------------------------------------------------------------------------

// TestUser_CanReach verifies membership answers only for granted buckets.
func TestUser_CanReach(t *testing.T) {
	t.Parallel()

	u := NewUser("u1", "ci", []string{"photos", "backups"})
	for _, want := range []string{"photos", "backups"} {
		if !u.CanReach(want) {
			t.Errorf("CanReach(%q) = false, want true", want)
		}
	}
	if u.CanReach("other") {
		t.Error("CanReach(other) = true, want false")
	}
}

// TestUser_NilReachesNothing verifies the zero case is safe. A request that
// failed to authenticate carries no user, and the transport asks the same
// question of it rather than branching first.
func TestUser_NilReachesNothing(t *testing.T) {
	t.Parallel()

	var u *User
	if u.CanReach("photos") {
		t.Error("a nil user reaches a bucket")
	}
	if got := u.Buckets(); got != nil {
		t.Errorf("Buckets() = %v, want nil", got)
	}
}

// TestUser_BucketsSorted verifies the listing is ordered, so a ListBuckets
// response does not reshuffle between requests as the map iterates.
func TestUser_BucketsSorted(t *testing.T) {
	t.Parallel()

	got := NewUser("u1", "ci", []string{"zeta", "alpha", "mid"}).Buckets()
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("Buckets() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Buckets() = %v, want %v", got, want)
		}
	}
}

// -------------------------------------------------------------------------
// CONFIG-DECLARED USERS
// -------------------------------------------------------------------------

// TestNewBucketRegistry_ConfigCredentialReachesItsBucket verifies a config
// credential resolves to a user granted exactly the bucket that declared it,
// which is what keeps the answer identical to the config-only behaviour.
func TestNewBucketRegistry_ConfigCredentialReachesItsBucket(t *testing.T) {
	t.Parallel()

	br := mustBucketRegistry(t, []config.BucketConfig{
		{Name: "photos", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AK", SecretAccessKey: "SK"},
		}},
		{Name: "backups", Credentials: []config.CredentialConfig{
			{AccessKeyID: "BK", SecretAccessKey: "BS"},
		}},
	})

	u := br.byAccessKey["AK"].user
	if !u.CanReach("photos") {
		t.Errorf("reaches %v, want photos", u.Buckets())
	}
	if u.CanReach("backups") {
		t.Error("a config credential reaches another bucket's namespace")
	}
	if !u.FromConfig {
		t.Error("FromConfig = false, want true for a config-declared credential")
	}
}

// TestNewBucketRegistry_ConfigUserIDIsStable verifies the identity a config
// credential resolves to is derived from its access key, so an audit record
// naming it means the same thing after a restart.
func TestNewBucketRegistry_ConfigUserIDIsStable(t *testing.T) {
	t.Parallel()

	buckets := []config.BucketConfig{
		{Name: "photos", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AK", SecretAccessKey: "SK"},
		}},
	}
	first := mustBucketRegistry(t, buckets).byAccessKey["AK"].user.ID
	second := mustBucketRegistry(t, buckets).byAccessKey["AK"].user.ID
	if first != second {
		t.Errorf("user id changed across assemblies: %q then %q", first, second)
	}
	if first == "" {
		t.Error("config credential resolved to an empty user id")
	}
}

// TestNewBucketRegistry_KeypairAndTokenShareOneUser verifies a credential
// carrying both proofs resolves to one identity, so either attributes the same
// actor.
func TestNewBucketRegistry_KeypairAndTokenShareOneUser(t *testing.T) {
	t.Parallel()

	br := mustBucketRegistry(t, []config.BucketConfig{
		{Name: "photos", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AK", SecretAccessKey: "SK", Token: "TOK"},
		}},
	})

	if br.byAccessKey["AK"].user != br.byToken["TOK"].user {
		t.Error("keypair and token resolved to different users")
	}
}

// -------------------------------------------------------------------------
// STORED CREDENTIALS
// -------------------------------------------------------------------------

// TestNewBucketRegistry_StoredCredentialAuthenticates verifies a keypair the
// store holds reaches the buckets its user was granted.
func TestNewBucketRegistry_StoredCredentialAuthenticates(t *testing.T) {
	t.Parallel()

	br := mustBucketRegistryWithStore(t, nil, &provisioning.Snapshot{
		Buckets:     []core.Bucket{{Name: "photos"}, {Name: "backups"}},
		Users:       []core.User{{ID: "u1", Name: "ci"}},
		Credentials: []core.Credential{{AccessKeyID: "STORED", UserID: "u1", Secret: "s1"}},
		Grants: []core.Grant{
			{UserID: "u1", BucketName: "photos"},
			{UserID: "u1", BucketName: "backups"},
		},
	})

	e, ok := br.byAccessKey["STORED"]
	if !ok {
		t.Fatal("stored credential missing from the registry")
	}
	if e.secret != "s1" {
		t.Errorf("secret = %q, want s1", e.secret)
	}
	for _, want := range []string{"photos", "backups"} {
		if !e.user.CanReach(want) {
			t.Errorf("reaches %v, want it to include %q", e.user.Buckets(), want)
		}
	}
	if e.user.FromConfig {
		t.Error("FromConfig = true, want false for a stored credential")
	}
}

// TestNewBucketRegistry_ConfigShadowsStoredCredential verifies an access key
// both sources declare keeps the config credential and reports the collision
// rather than refusing to start. A deployment reaches that state by editing a
// file, and refusing would take the fleet down over it.
func TestNewBucketRegistry_ConfigShadowsStoredCredential(t *testing.T) {
	t.Parallel()

	br := mustBucketRegistryWithStore(t,
		[]config.BucketConfig{
			{Name: "photos", Credentials: []config.CredentialConfig{
				{AccessKeyID: "AK", SecretAccessKey: "config-secret"},
			}},
		},
		&provisioning.Snapshot{
			Buckets:     []core.Bucket{{Name: "backups"}},
			Users:       []core.User{{ID: "u1", Name: "ci"}},
			Credentials: []core.Credential{{AccessKeyID: "AK", UserID: "u1", Secret: "stored-secret"}},
			Grants:      []core.Grant{{UserID: "u1", BucketName: "backups"}},
		},
	)

	e := br.byAccessKey["AK"]
	if e.secret != "config-secret" {
		t.Errorf("secret = %q, want the config one", e.secret)
	}
	if e.user.CanReach("backups") {
		t.Error("the stored credential's grants leaked into the config identity")
	}
	if n := len(br.Notices()); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}
	if br.Notices()[0].Kind != provisioning.NoticeCredentialShadowed {
		t.Errorf("notice kind = %q, want %q", br.Notices()[0].Kind, provisioning.NoticeCredentialShadowed)
	}
}

// TestNewBucketRegistry_IncompleteStoredCredentialIgnored verifies a row missing
// the parts that make it usable never becomes an authenticating credential.
func TestNewBucketRegistry_IncompleteStoredCredentialIgnored(t *testing.T) {
	t.Parallel()

	br := mustBucketRegistryWithStore(t, nil, &provisioning.Snapshot{
		Users: []core.User{{ID: "u1", Name: "a"}, {ID: "u2", Name: "b"}},
		Credentials: []core.Credential{
			{AccessKeyID: "", UserID: "u1", Secret: "s"},
			{AccessKeyID: "NO_SECRET", UserID: "u2", Secret: ""},
			{AccessKeyID: "NO_USER", UserID: "gone", Secret: "s"},
		},
	})

	if n := len(br.byAccessKey); n != 0 {
		t.Errorf("registered %d credentials, want none", n)
	}
}

// TestAuthenticate_StoredCredentialSignsRequests verifies a stored keypair
// verifies a real SigV4 signature, which is the whole point of reading the
// secret back rather than hashing it.
func TestAuthenticate_StoredCredentialSignsRequests(t *testing.T) {
	t.Parallel()

	br := mustBucketRegistryWithStore(t, nil, &provisioning.Snapshot{
		Buckets:     []core.Bucket{{Name: "photos"}},
		Users:       []core.User{{ID: "u1", Name: "ci"}},
		Credentials: []core.Credential{{AccessKeyID: "STORED", UserID: "u1", Secret: "stored-secret"}},
		Grants:      []core.Grant{{UserID: "u1", BucketName: "photos"}},
	})

	r := signRequest(t, http.MethodGet, "/photos/test.txt", "STORED", "stored-secret")
	u, _, err := br.Authenticate(r)
	if err != nil {
		t.Fatalf("stored credential should authenticate: %v", err)
	}
	if !u.CanReach("photos") {
		t.Errorf("reaches %v, want photos", u.Buckets())
	}
}
