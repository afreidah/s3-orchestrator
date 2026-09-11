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
}

// TestMerge_BucketCORSCarriesAcross verifies a bucket's CORS rules survive the
// merge from either source, since the browser policy is compiled from them.
func TestMerge_BucketCORSCarriesAcross(t *testing.T) {
	t.Parallel()

	rule := []config.CORSRule{{AllowedOrigins: []string{"https://example.com"}, AllowedMethods: []string{"GET"}}}
	v := Merge(
		[]config.BucketConfig{{Name: "cfg", CORS: rule}},
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
	}, &Snapshot{})

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

// TestMerge_KeypairAndTokenShareOneCredential verifies a config credential
// carrying both proofs stays one credential proving one user, so either
// attributes the same actor.
func TestMerge_KeypairAndTokenShareOneCredential(t *testing.T) {
	t.Parallel()

	v := Merge([]config.BucketConfig{
		{Name: "photos", Credentials: []config.CredentialConfig{
			{AccessKeyID: "AK", SecretAccessKey: "SK", Token: "TOK"},
		}},
	}, &Snapshot{})

	if len(v.Credentials) != 1 {
		t.Fatalf("merged %d credentials, want 1", len(v.Credentials))
	}
	c := v.Credentials[0]
	if c.AccessKeyID != "AK" || c.Secret != "SK" || c.Token != "TOK" {
		t.Errorf("credential = %+v, want both proofs carried", c)
	}
}

// TestConfigUserID_FallsBackToPosition verifies a token-only credential, which
// has no public identifier, still names a stable user.
func TestConfigUserID_FallsBackToPosition(t *testing.T) {
	t.Parallel()

	if got := ConfigUserID("photos", 0, "AK"); got != "config:AK" {
		t.Errorf("ConfigUserID with an access key = %q, want config:AK", got)
	}
	if got := ConfigUserID("photos", 2, ""); got != "config:photos:2" {
		t.Errorf("ConfigUserID without an access key = %q, want config:photos:2", got)
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
		&Snapshot{
			Buckets:     []core.Bucket{{Name: "store-bucket"}},
			Users:       []core.User{{ID: "u1", Name: "ci"}},
			Credentials: []core.Credential{{AccessKeyID: "AK1", UserID: "u1", Secret: "s1"}},
			Grants: []core.Grant{
				{UserID: "u1", BucketName: "store-bucket"},
				{UserID: "u1", BucketName: "config-bucket"},
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

	v := Merge(nil, &Snapshot{
		Buckets: []core.Bucket{{Name: "zeta"}, {Name: "alpha"}, {Name: "mid"}},
		Users:   []core.User{{ID: "u1", Name: "ci"}},
		Grants: []core.Grant{
			{UserID: "u1", BucketName: "zeta"},
			{UserID: "u1", BucketName: "alpha"},
			{UserID: "u1", BucketName: "mid"},
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

	v := Merge(nil, &Snapshot{
		Users:       []core.User{{ID: "u1", Name: "ci"}},
		Credentials: []core.Credential{{AccessKeyID: "AK1", UserID: "u1", Secret: "s1"}},
		Grants:      []core.Grant{{UserID: "u1", BucketName: "gone"}},
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

	v := Merge(nil, &Snapshot{
		Buckets: []core.Bucket{{Name: "b"}},
		Users:   []core.User{{ID: "u1", Name: "ci"}},
		Credentials: []core.Credential{
			{AccessKeyID: "LIVE", UserID: "u1", Secret: "s1"},
			{AccessKeyID: "OFF", UserID: "u1", Secret: "s2", Disabled: true},
		},
		Grants: []core.Grant{{UserID: "u1", BucketName: "b"}},
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

	v := Merge(nil, &Snapshot{
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
