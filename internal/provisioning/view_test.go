// -------------------------------------------------------------------------------
// Provisioning - Merge Tests
//
// Author: Alex Freidah
//
// Covers the fold of the store's rows into what config declares: config winning
// a name collision, grants joining onto their user, a grant naming a bucket
// nothing declares, and the credential states that resolve to nothing.
//
// Merge is pure, so these run without a store.
// -------------------------------------------------------------------------------

package provisioning

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// bucketNames lists the merged bucket names in the order the merge produced.
func bucketNames(buckets []Bucket) []string {
	out := make([]string, 0, len(buckets))
	for i := range buckets {
		out = append(out, buckets[i].Name)
	}
	return out
}

// findCredential returns the merged credential for an access key.
func findCredential(creds []Credential, accessKey string) (Credential, bool) {
	for _, c := range creds {
		if c.AccessKeyID == accessKey {
			return c, true
		}
	}
	return Credential{}, false
}

// findUser returns the merged user with an id.
func findUser(users []User, id string) (User, bool) {
	for _, u := range users {
		if u.ID == id {
			return u, true
		}
	}
	return User{}, false
}

// countNotices counts merge notices of one kind.
func countNotices(notices []Notice, kind string) int {
	n := 0
	for _, notice := range notices {
		if notice.Kind == kind {
			n++
		}
	}
	return n
}

// reaches reports whether a user's bucket list contains a name.
func reaches(u *User, bucket string) bool {
	for _, b := range u.Buckets {
		if b == bucket {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------------
// BUCKETS
// -------------------------------------------------------------------------

// TestMerge_StoredBucketsFollowConfig verifies a stored bucket joins the merged
// list, carries its own settings across, and is marked as coming from the store.
func TestMerge_StoredBucketsFollowConfig(t *testing.T) {
	t.Parallel()

	v := Merge(
		[]config.BucketConfig{{Name: "from-config"}},
		config.AuthConfig{},
		&Snapshot{Buckets: []core.Bucket{{Name: "from-store", MaxMultipartUploads: 7}}},
	)

	got := bucketNames(v.Buckets)
	if len(got) != 2 || got[0] != "from-config" || got[1] != "from-store" {
		t.Fatalf("merged buckets = %v, want [from-config from-store]", got)
	}
	if v.Buckets[0].Source != SourceConfig {
		t.Errorf("config bucket source = %q, want %q", v.Buckets[0].Source, SourceConfig)
	}
	if v.Buckets[1].Source != SourceStore {
		t.Errorf("stored bucket source = %q, want %q", v.Buckets[1].Source, SourceStore)
	}
	if v.Buckets[1].MaxMultipartUploads != 7 {
		t.Errorf("stored bucket limit = %d, want 7", v.Buckets[1].MaxMultipartUploads)
	}
}

// TestMerge_ConfigWinsNameCollision verifies a stored bucket sharing a config
// bucket's name is dropped and reported, so an operator reading the config file
// can trust what it says.
func TestMerge_ConfigWinsNameCollision(t *testing.T) {
	t.Parallel()

	v := Merge(
		[]config.BucketConfig{{Name: "photos", MaxMultipartUploads: 1}},
		config.AuthConfig{},
		&Snapshot{Buckets: []core.Bucket{{Name: "photos", MaxMultipartUploads: 99}}},
	)

	if got := bucketNames(v.Buckets); len(got) != 1 || got[0] != "photos" {
		t.Fatalf("merged buckets = %v, want [photos]", got)
	}
	if v.Buckets[0].MaxMultipartUploads != 1 {
		t.Errorf("config bucket was overwritten by the stored one: limit = %d, want 1",
			v.Buckets[0].MaxMultipartUploads)
	}
	if n := countNotices(v.Notices, NoticeBucketShadowed); n != 1 {
		t.Errorf("shadowing notices = %d, want 1", n)
	}
	if len(v.Shadowed) != 1 || v.Shadowed[0].Name != "photos" {
		t.Fatalf("shadowed = %v, want the stored row reported", v.Shadowed)
	}
	if v.Shadowed[0].MaxMultipartUploads != 99 {
		t.Errorf("shadowed limit = %d, want the stored row's 99",
			v.Shadowed[0].MaxMultipartUploads)
	}
	if v.Shadowed[0].Source != SourceStore {
		t.Errorf("shadowed source = %q, want %q", v.Shadowed[0].Source, SourceStore)
	}
}

// TestMerge_UnshadowedBucketIsNotReportedTwice verifies a stored bucket no
// config bucket collides with is merged normally and stays out of Shadowed,
// which is what makes Shadowed mean "inert" rather than "stored".
func TestMerge_UnshadowedBucketIsNotReportedTwice(t *testing.T) {
	t.Parallel()

	v := Merge(
		[]config.BucketConfig{{Name: "photos"}},
		config.AuthConfig{},
		&Snapshot{Buckets: []core.Bucket{{Name: "backups"}}},
	)

	if len(v.Shadowed) != 0 {
		t.Errorf("shadowed = %v, want none", v.Shadowed)
	}
	if got := bucketNames(v.Buckets); len(got) != 2 {
		t.Errorf("merged buckets = %v, want both", got)
	}
}

// TestMerge_BucketCORSCarriesAcross verifies a bucket's CORS rules survive the
// merge from either source, since the browser policy is compiled from them.
func TestMerge_BucketCORSCarriesAcross(t *testing.T) {
	t.Parallel()

	rule := []config.CORSRule{{AllowedOrigins: []string{"https://example.com"}, AllowedMethods: []string{"GET"}}}
	v := Merge(
		[]config.BucketConfig{{Name: "cfg", CORS: rule}},
		config.AuthConfig{},
		&Snapshot{Buckets: []core.Bucket{{Name: "sto", CORS: rule}}},
	)

	for i := range v.Buckets {
		if len(v.Buckets[i].CORS) != 1 {
			t.Errorf("bucket %q lost its CORS rules", v.Buckets[i].Name)
		}
	}
}

// -------------------------------------------------------------------------
// CONFIG CREDENTIALS
// -------------------------------------------------------------------------

// TestMerge_ConfigCredentialBecomesAUser verifies a config-declared credential
// resolves to a user reaching exactly the bucket that declared it.
func TestMerge_ConfigCredentialBecomesAUser(t *testing.T) {
	t.Parallel()

	v := Merge([]config.BucketConfig{
		{Name: "photos", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AK", SecretAccessKey: "SK"},
		}},
		{Name: "backups", Credentials: []config.CredentialConfig{
			{AccessKeyID: "BK", SecretAccessKey: "BS"},
		}},
	}, config.AuthConfig{}, &Snapshot{})

	cred, ok := findCredential(v.Credentials, "AK")
	if !ok {
		t.Fatal("AK missing from the merged credentials")
	}
	if cred.Source != SourceConfig {
		t.Errorf("source = %q, want %q", cred.Source, SourceConfig)
	}
	u, ok := findUser(v.Users, cred.UserID)
	if !ok {
		t.Fatalf("credential names user %q, which the view does not carry", cred.UserID)
	}
	if !reaches(&u, "photos") {
		t.Errorf("user reaches %v, want photos", u.Buckets)
	}
	if reaches(&u, "backups") {
		t.Error("a config credential reaches another bucket's namespace")
	}
}

// TestMerge_ConfigCredentialCarriesItsKeypair verifies a config credential
// reaches the registry with both halves, which is what the request path repeats
// the caller's SigV4 derivation with.
func TestMerge_ConfigCredentialCarriesItsKeypair(t *testing.T) {
	t.Parallel()

	v := Merge([]config.BucketConfig{
		{Name: "photos", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AK", SecretAccessKey: "SK"},
		}},
	}, config.AuthConfig{}, &Snapshot{})

	if len(v.Credentials) != 1 {
		t.Fatalf("merged %d credentials, want 1", len(v.Credentials))
	}
	c := v.Credentials[0]
	if c.AccessKeyID != "AK" || c.Secret != "SK" {
		t.Errorf("credential = %+v, want the keypair carried", c)
	}
}

// TestConfigUserID_NamesTheAccessKey verifies the id a config credential
// resolves to is its access key, which makes it stable across restarts and
// across reordering a bucket's credential list.
func TestConfigUserID_NamesTheAccessKey(t *testing.T) {
	t.Parallel()

	if got := ConfigUserID("AK"); got != "config:AK" {
		t.Errorf("ConfigUserID = %q, want config:AK", got)
	}
}

// -------------------------------------------------------------------------
// STORE ROWS
// -------------------------------------------------------------------------

// TestMerge_GrantsJoinOntoUser verifies a user reaches every bucket it holds a
// grant on, including one the config file declares rather than the store.
func TestMerge_GrantsJoinOntoUser(t *testing.T) {
	t.Parallel()

	v := Merge(
		[]config.BucketConfig{{Name: "config-bucket"}},
		config.AuthConfig{},
		&Snapshot{
			Buckets:     []core.Bucket{{Name: "store-bucket"}},
			Users:       []core.User{{ID: "u1", Name: "ci"}},
			Credentials: []core.Credential{{AccessKeyID: "AK1", UserID: "u1", Secret: "s1"}},
			Grants: []core.Grant{
				{UserID: "u1", Resource: core.BucketResource("store-bucket")},
				{UserID: "u1", Resource: core.BucketResource("config-bucket")},
			},
		},
	)

	cred, ok := findCredential(v.Credentials, "AK1")
	if !ok {
		t.Fatal("AK1 missing from the merged credentials")
	}
	if cred.Secret != "s1" {
		t.Errorf("secret = %q, want s1", cred.Secret)
	}
	u, ok := findUser(v.Users, "u1")
	if !ok {
		t.Fatal("u1 missing from the merged users")
	}
	for _, want := range []string{"store-bucket", "config-bucket"} {
		if !reaches(&u, want) {
			t.Errorf("user reaches %v, want it to include %q", u.Buckets, want)
		}
	}
}

// TestMerge_UserBucketsSorted verifies a user's reach is ordered, so a listing
// does not reshuffle between reads as the grant rows arrive.
func TestMerge_UserBucketsSorted(t *testing.T) {
	t.Parallel()

	v := Merge(nil, config.AuthConfig{}, &Snapshot{
		Buckets: []core.Bucket{{Name: "zeta"}, {Name: "alpha"}, {Name: "mid"}},
		Users:   []core.User{{ID: "u1", Name: "ci"}},
		Grants: []core.Grant{
			{UserID: "u1", Resource: core.BucketResource("zeta")},
			{UserID: "u1", Resource: core.BucketResource("alpha")},
			{UserID: "u1", Resource: core.BucketResource("mid")},
		},
	})

	u, _ := findUser(v.Users, "u1")
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if u.Buckets[i] != want[i] {
			t.Fatalf("buckets = %v, want %v", u.Buckets, want)
		}
	}
}

// TestMerge_DanglingGrantReported verifies a grant naming a bucket neither
// source declares is skipped and reported rather than failing the merge. A
// config bucket can be removed while a grant to it survives in the store.
func TestMerge_DanglingGrantReported(t *testing.T) {
	t.Parallel()

	v := Merge(nil, config.AuthConfig{}, &Snapshot{
		Users:       []core.User{{ID: "u1", Name: "ci"}},
		Credentials: []core.Credential{{AccessKeyID: "AK1", UserID: "u1", Secret: "s1"}},
		Grants:      []core.Grant{{UserID: "u1", Resource: core.BucketResource("gone")}},
	})

	if n := countNotices(v.Notices, NoticeDanglingGrant); n != 1 {
		t.Fatalf("dangling-grant notices = %d, want 1", n)
	}
	if _, ok := findCredential(v.Credentials, "AK1"); !ok {
		t.Fatal("AK1 missing: a dangling grant must not drop the credential")
	}
	u, _ := findUser(v.Users, "u1")
	if reaches(&u, "gone") {
		t.Error("user reaches a bucket nothing declares")
	}
}

// TestMerge_DisabledCredentialOmitted verifies a disabled credential never
// reaches the view, which is what makes disabling one take effect while its row
// survives for the record of what it did.
func TestMerge_DisabledCredentialOmitted(t *testing.T) {
	t.Parallel()

	v := Merge(nil, config.AuthConfig{}, &Snapshot{
		Buckets: []core.Bucket{{Name: "b"}},
		Users:   []core.User{{ID: "u1", Name: "ci"}},
		Credentials: []core.Credential{
			{AccessKeyID: "LIVE", UserID: "u1", Secret: "s1"},
			{AccessKeyID: "OFF", UserID: "u1", Secret: "s2", Disabled: true},
		},
		Grants: []core.Grant{{UserID: "u1", Resource: core.BucketResource("b")}},
	})

	if _, ok := findCredential(v.Credentials, "OFF"); ok {
		t.Error("a disabled credential reached the view")
	}
	if _, ok := findCredential(v.Credentials, "LIVE"); !ok {
		t.Error("disabling one credential removed its sibling")
	}
}

// TestMerge_CredentialWithoutUserOmitted verifies a credential whose user is
// absent resolves to nothing rather than to a user with no identity.
func TestMerge_CredentialWithoutUserOmitted(t *testing.T) {
	t.Parallel()

	v := Merge(nil, config.AuthConfig{}, &Snapshot{
		Credentials: []core.Credential{{AccessKeyID: "AK1", UserID: "missing", Secret: "s1"}},
	})

	if len(v.Credentials) != 0 {
		t.Errorf("merged %d credentials, want none", len(v.Credentials))
	}
}

// TestMerge_UserWithoutGrantsReachesNothing verifies a user holding no grants
// still resolves and is refused on every bucket, which is a clearer answer than
// a failed signature for a user granted nothing yet.
func TestMerge_UserWithoutGrantsReachesNothing(t *testing.T) {
	t.Parallel()

	v := Merge(
		[]config.BucketConfig{{Name: "photos"}},
		config.AuthConfig{},
		&Snapshot{
			Users:       []core.User{{ID: "u1", Name: "new"}},
			Credentials: []core.Credential{{AccessKeyID: "AK1", UserID: "u1", Secret: "s1"}},
		},
	)

	if _, ok := findCredential(v.Credentials, "AK1"); !ok {
		t.Fatal("AK1 missing from the merged credentials")
	}
	u, ok := findUser(v.Users, "u1")
	if !ok {
		t.Fatal("u1 missing from the merged users")
	}
	if len(u.Buckets) != 0 {
		t.Errorf("user reaches %v, want nothing", u.Buckets)
	}
}

// wildcardGrant is the grant an operator holds to reach every bucket, including
// ones created after it was written.
func wildcardGrant(user string, perms core.PermissionSet) core.Grant {
	return core.Grant{
		UserID:      user,
		Resource:    core.Resource{Kind: core.ResourceBucket, Name: core.ResourceWildcard},
		Permissions: perms,
	}
}

// TestMerge_WildcardReachesEveryBucket asserts a wildcard grant covers buckets
// it never names, which is the point of it: a bucket created tomorrow is
// reachable without anyone rewriting grants.
func TestMerge_WildcardReachesEveryBucket(t *testing.T) {
	t.Parallel()

	v := Merge(nil, config.AuthConfig{}, &Snapshot{
		Buckets: []core.Bucket{{Name: "photos"}, {Name: "logs"}},
		Users:   []core.User{{ID: "u1", Name: "devops"}},
		Grants:  []core.Grant{wildcardGrant("u1", core.PermRead)},
	})

	u, ok := findUser(v.Users, "u1")
	if !ok {
		t.Fatal("u1 missing from the merged users")
	}
	for _, bucket := range []string{"photos", "logs"} {
		if got := u.Grants[bucket]; got != core.PermRead {
			t.Errorf("%s = %q, want read", bucket, got)
		}
	}
}

// TestMerge_NamedGrantNarrowsTheWildcard is the carve-out case: broad access
// with one bucket cut down. A named grant replaces the wildcard rather than
// adding to it, or the narrower grant would be unable to say anything.
func TestMerge_NamedGrantNarrowsTheWildcard(t *testing.T) {
	t.Parallel()

	v := Merge(nil, config.AuthConfig{}, &Snapshot{
		Buckets: []core.Bucket{{Name: "photos"}, {Name: "secrets"}},
		Users:   []core.User{{ID: "u1", Name: "devops"}},
		Grants: []core.Grant{
			wildcardGrant("u1", core.PermRead|core.PermWrite),
			{UserID: "u1", Resource: core.BucketResource("secrets"), Permissions: core.PermRead},
		},
	})

	u, _ := findUser(v.Users, "u1")
	if got := u.Grants["photos"]; got != core.PermRead|core.PermWrite {
		t.Errorf("photos = %q, want read,write from the wildcard", got)
	}
	if got := u.Grants["secrets"]; got != core.PermRead {
		t.Errorf("secrets = %q, want read - the named grant replaces the wildcard", got)
	}
}

// TestMerge_NamedGrantWidensTheWildcard covers the other direction, so the rule
// is precedence rather than "narrowest wins".
func TestMerge_NamedGrantWidensTheWildcard(t *testing.T) {
	t.Parallel()

	v := Merge(nil, config.AuthConfig{}, &Snapshot{
		Buckets: []core.Bucket{{Name: "photos"}, {Name: "uploads"}},
		Users:   []core.User{{ID: "u1", Name: "devops"}},
		Grants: []core.Grant{
			wildcardGrant("u1", core.PermRead),
			{UserID: "u1", Resource: core.BucketResource("uploads"), Permissions: core.PermRead | core.PermWrite | core.PermDelete},
		},
	})

	u, _ := findUser(v.Users, "u1")
	if got := u.Grants["uploads"]; got != core.PermRead|core.PermWrite|core.PermDelete {
		t.Errorf("uploads = %q, want the named grant's permissions", got)
	}
	if got := u.Grants["photos"]; got != core.PermRead {
		t.Errorf("photos = %q, want read from the wildcard", got)
	}
}

// TestMerge_ControlPlaneGrantsStayOutOfTheBucketLookup asserts a backend or
// instance grant reaches no bucket. They authorize the control plane, which this
// lookup answers nothing about.
func TestMerge_ControlPlaneGrantsStayOutOfTheBucketLookup(t *testing.T) {
	t.Parallel()

	v := Merge(nil, config.AuthConfig{}, &Snapshot{
		Buckets: []core.Bucket{{Name: "photos"}},
		Users:   []core.User{{ID: "u1", Name: "devops"}},
		Grants: []core.Grant{
			{UserID: "u1", Resource: core.Resource{Kind: core.ResourceBackend, Name: core.ResourceWildcard}, Permissions: core.PermAdminDrain},
			{UserID: "u1", Resource: core.Resource{Kind: core.ResourceOrchestrator}, Permissions: core.PermAdminProvision},
		},
	})

	u, _ := findUser(v.Users, "u1")
	if len(u.Grants) != 0 {
		t.Errorf("bucket grants = %v, want none from control-plane grants", u.Grants)
	}
}
