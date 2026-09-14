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

	u := NewUser("u1", "ci", grantsOn("photos", "backups"))
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

	got := NewUser("u1", "ci", grantsOn("zeta", "alpha", "mid")).Buckets()
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

// TestNewBucketRegistry_SiblingKeypairsShareOneUser verifies two keypairs a
// bucket declares resolve to one identity, so either attributes the same actor
// and revoking one leaves the other working.
func TestNewBucketRegistry_SiblingKeypairsShareOneUser(t *testing.T) {
	t.Parallel()

	br := mustBucketRegistry(t, []config.BucketConfig{
		{Name: "photos", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AK1", SecretAccessKey: "SK1"},
			{AccessKeyID: "AK2", SecretAccessKey: "SK2"},
		}},
	})

	first, second := br.byAccessKey["AK1"].user, br.byAccessKey["AK2"].user
	if first == nil || second == nil {
		t.Fatal("a declared keypair did not register")
	}
	if !first.CanReach("photos") || !second.CanReach("photos") {
		t.Error("sibling keypairs did not both reach the bucket that declared them")
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
			{UserID: "u1", Resource: core.BucketResource("photos")},
			{UserID: "u1", Resource: core.BucketResource("backups")},
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
			Grants:      []core.Grant{{UserID: "u1", Resource: core.BucketResource("backups")}},
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
		Grants:      []core.Grant{{UserID: "u1", Resource: core.BucketResource("photos")}},
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

// -------------------------------------------------------------------------
// PERMISSIONS
// -------------------------------------------------------------------------

// grantsOn builds a grant set carrying every permission on each named bucket,
// which is what a grant recording none holds.
func grantsOn(buckets ...string) map[string]core.PermissionSet {
	out := make(map[string]core.PermissionSet, len(buckets))
	for _, b := range buckets {
		out[b] = core.PermAll
	}
	return out
}

// TestUser_Can verifies a grant authorizes exactly what it carries, and that a
// bucket the user does not reach at all authorizes nothing.
func TestUser_Can(t *testing.T) {
	t.Parallel()

	u := NewUser("u1", "ci", map[string]core.PermissionSet{
		"readonly": core.PermListBuckets | core.PermList | core.PermRead,
		"full":     core.PermAll,
	})

	for _, tc := range []struct {
		name   string
		bucket string
		want   core.PermissionSet
		ok     bool
	}{
		{"read on a read-only grant", "readonly", core.PermRead, true},
		{"list on a read-only grant", "readonly", core.PermList, true},
		{"write on a read-only grant", "readonly", core.PermWrite, false},
		{"delete on a read-only grant", "readonly", core.PermDelete, false},
		{"write on a full grant", "full", core.PermWrite, true},
		{"read and write together", "full", core.PermRead | core.PermWrite, true},
		{"read and write on read-only", "readonly", core.PermRead | core.PermWrite, false},
		{"anything on an ungranted bucket", "other", core.PermRead, false},
		{"nothing required still needs the grant", "other", 0, false},
		{"nothing required on a held grant", "readonly", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := u.Can(tc.bucket, tc.want); got != tc.ok {
				t.Errorf("Can(%q, %v) = %t, want %t", tc.bucket, tc.want, got, tc.ok)
			}
		})
	}
}

// TestUser_CanNil verifies a caller that failed to authenticate is refused
// rather than panicking on the check.
func TestUser_CanNil(t *testing.T) {
	t.Parallel()

	var u *User
	if u.Can("photos", core.PermRead) {
		t.Error("a nil user was authorized")
	}
	if _, ok := u.Permissions("photos"); ok {
		t.Error("a nil user reported holding a grant")
	}
}

// TestUser_CanReachIsNotCan pins the split the two refusals rest on: a grant
// that carries nothing the operation needs still reaches the bucket, and the
// caller is told it may not act rather than that the bucket does not exist.
func TestUser_CanReachIsNotCan(t *testing.T) {
	t.Parallel()

	u := NewUser("u1", "ci", map[string]core.PermissionSet{"photos": core.PermRead})
	if !u.CanReach("photos") {
		t.Error("a read-only grant does not reach its bucket")
	}
	if u.Can("photos", core.PermDelete) {
		t.Error("a read-only grant authorized a delete")
	}
}

// TestUser_BucketsNeedsListBuckets verifies a bucket the caller may not see
// listed is left out of ListBuckets, and one it may is included whatever else
// the grant withholds.
func TestUser_BucketsNeedsListBuckets(t *testing.T) {
	t.Parallel()

	u := NewUser("u1", "ci", map[string]core.PermissionSet{
		"listed":   core.PermListBuckets | core.PermWrite,
		"unlisted": core.PermWrite,
	})

	got := u.Buckets()
	if len(got) != 1 || got[0] != "listed" {
		t.Errorf("Buckets() = %v, want only the bucket carrying list-buckets", got)
	}
}

// TestUser_CanAdminFallsBackToTheWildcard verifies a grant on one backend
// answers for that backend only, while the kind's wildcard answers for every
// backend, including one the deployment has not added yet.
func TestUser_CanAdminFallsBackToTheWildcard(t *testing.T) {
	t.Parallel()

	named := NewUser("u1", "ci", nil).WithAdmin(map[core.Resource]core.PermissionSet{
		{Kind: core.ResourceBackend, Name: "b1"}: core.PermAdminDrain,
	})
	wild := NewUser("u2", "ops", nil).WithAdmin(map[core.Resource]core.PermissionSet{
		{Kind: core.ResourceBackend, Name: core.ResourceWildcard}: core.PermAdminDrain,
	})

	b1 := core.Resource{Kind: core.ResourceBackend, Name: "b1"}
	b2 := core.Resource{Kind: core.ResourceBackend, Name: "b2"}
	for _, tc := range []struct {
		name string
		user *User
		on   core.Resource
		want bool
	}{
		{"named grant on its backend", named, b1, true},
		{"named grant on another backend", named, b2, false},
		{"wildcard on a named backend", wild, b1, true},
		{"wildcard on every other backend", wild, b2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.user.CanAdmin(tc.on, core.PermAdminDrain); got != tc.want {
				t.Errorf("CanAdmin(%v) = %v, want %v", tc.on, got, tc.want)
			}
		})
	}
}

// TestUser_CanAdminRefusesWithoutAGrant verifies nothing is granted implicitly,
// which is what keeps an S3 credential out of the control plane: bucket grants
// say nothing about draining a backend.
func TestUser_CanAdminRefusesWithoutAGrant(t *testing.T) {
	t.Parallel()

	u := NewUser("u1", "ci", map[string]core.PermissionSet{"photos": core.PermAll})
	for _, on := range []core.Resource{
		{Kind: core.ResourceOrchestrator},
		{Kind: core.ResourceBackend, Name: "b1"},
		{Kind: core.ResourceBackend, Name: core.ResourceWildcard},
	} {
		if u.CanAdmin(on, core.PermAdminRead) {
			t.Errorf("a bucket grant authorized %v", on)
		}
	}
	if (*User)(nil).CanAdmin(core.Resource{Kind: core.ResourceOrchestrator}, core.PermAdminRead) {
		t.Error("a nil user authorized a control-plane operation")
	}
}

// TestUser_CanAdminHoldsPermissionsApart verifies one control-plane permission
// does not carry the others, which is the reason the vocabulary has more than
// one bit.
func TestUser_CanAdminHoldsPermissionsApart(t *testing.T) {
	t.Parallel()

	instance := core.Resource{Kind: core.ResourceOrchestrator}
	u := NewUser("u1", "monitor", nil).WithAdmin(map[core.Resource]core.PermissionSet{
		instance: core.PermAdminRead,
	})

	if !u.CanAdmin(instance, core.PermAdminRead) {
		t.Error("the granted permission was refused")
	}
	for _, want := range []core.PermissionSet{
		core.PermAdminLogs, core.PermAdminKeys, core.PermAdminProvision, core.PermAdminRead | core.PermAdminLogs,
	} {
		if u.CanAdmin(instance, want) {
			t.Errorf("a read-only grant authorized %q", want)
		}
	}
}

// TestUser_AllBucketsIsTheWildcard verifies the wildcard is carried and
// reported, which is what authorizes an operation spanning the namespace.
func TestUser_AllBucketsIsTheWildcard(t *testing.T) {
	t.Parallel()

	plain := NewUser("u1", "ci", map[string]core.PermissionSet{"photos": core.PermRead})
	if got := plain.AllBuckets(); got != 0 {
		t.Errorf("a user holding no wildcard reported %q", got)
	}

	wild := NewUser("u2", "ops", nil).WithAllBuckets(core.PermList | core.PermRead)
	if got := wild.AllBuckets(); got != core.PermList|core.PermRead {
		t.Errorf("AllBuckets() = %q, want list,read", got)
	}
	if (*User)(nil).AllBuckets() != 0 {
		t.Error("a nil user reported a wildcard")
	}
}

// TestUser_WildcardReachesAnUnnamedBucket verifies the wildcard answers for a
// bucket no grant names, while a named grant still answers alone so broad
// access can be carved down on one bucket.
func TestUser_WildcardReachesAnUnnamedBucket(t *testing.T) {
	t.Parallel()

	u := NewUser("u1", "ops", map[string]core.PermissionSet{
		"secrets": core.PermListBuckets,
	}).WithAllBuckets(core.PermAll)

	if !u.CanReach("anything") || !u.Can("anything", core.PermWrite) {
		t.Error("the wildcard did not reach a bucket no grant names")
	}
	if u.Can("secrets", core.PermWrite) {
		t.Error("the wildcard widened a named grant that carved it down")
	}
	held, ok := u.Permissions("anything")
	if !ok || held != core.PermAll {
		t.Errorf("Permissions(unnamed) = %q,%v, want the wildcard", held, ok)
	}
	if held, _ := u.Permissions("secrets"); held != core.PermListBuckets {
		t.Errorf("Permissions(named) = %q, want the named grant alone", held)
	}
}
