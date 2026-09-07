// -------------------------------------------------------------------------------
// DI - Bucket Registry Assembly Tests
//
// Author: Alex Freidah
//
// Covers the merge of the store's provisioning rows with what config declares:
// config winning a name collision, grants joining onto their user, a grant
// naming a bucket nothing declares, and the credential states that resolve to
// nothing.
//
// mergeProvisioned is pure, so these run without a store or an injector.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"testing"

	"github.com/samber/do/v2"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// emptyProvisioningStore answers every listing empty, which is the state a
// deployment that has provisioned nothing is in. Shared by the tests that need
// assembly to run without caring what the store holds.
func emptyProvisioningStore(t *testing.T) *storetest.MockProvisioningStore {
	t.Helper()
	s := storetest.NewMockProvisioningStore(gomock.NewController(t))
	a := gomock.Any()
	s.EXPECT().ListBuckets(a).Return(nil, nil).AnyTimes()
	s.EXPECT().ListUsers(a).Return(nil, nil).AnyTimes()
	s.EXPECT().ListCredentials(a).Return(nil, nil).AnyTimes()
	s.EXPECT().ListGrants(a).Return(nil, nil).AnyTimes()
	return s
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// bucketNames lists the merged bucket names in the order assembly produced.
func bucketNames(buckets []config.BucketConfig) []string {
	out := make([]string, 0, len(buckets))
	for i := range buckets {
		out = append(out, buckets[i].Name)
	}
	return out
}

// findCredential returns the assembled credential for an access key.
func findCredential(creds []auth.StoredCredential, accessKey string) (auth.StoredCredential, bool) {
	for _, c := range creds {
		if c.AccessKeyID == accessKey {
			return c, true
		}
	}
	return auth.StoredCredential{}, false
}

// countNotices counts assembled notices of one kind.
func countNotices(notices []auth.Notice, kind string) int {
	n := 0
	for _, notice := range notices {
		if notice.Kind == kind {
			n++
		}
	}
	return n
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// -------------------------------------------------------------------------
// ASSEMBLY
// -------------------------------------------------------------------------

// TestAssembleBucketRegistry_MergesBothSources verifies the exported entry point
// builds a registry a stored keypair authenticates against, which is what a boot
// and a reload both call.
func TestAssembleBucketRegistry_MergesBothSources(t *testing.T) {
	t.Parallel()

	store := storetest.NewMockProvisioningStore(gomock.NewController(t))
	a := gomock.Any()
	store.EXPECT().ListBuckets(a).Return([]core.Bucket{{Name: "stored"}}, nil).AnyTimes()
	store.EXPECT().ListUsers(a).Return([]core.User{{ID: "u1", Name: "ci"}}, nil).AnyTimes()
	store.EXPECT().ListCredentials(a).
		Return([]core.Credential{{AccessKeyID: "AK", UserID: "u1", Secret: "SK"}}, nil).AnyTimes()
	store.EXPECT().ListGrants(a).
		Return([]core.Grant{{UserID: "u1", BucketName: "stored"}}, nil).AnyTimes()

	inj := do.New()
	do.ProvideValue[core.ProvisioningStore](inj, store)

	reg, err := AssembleBucketRegistry(context.Background(), inj,
		&config.Config{Buckets: []config.BucketConfig{{Name: "from-config"}}})
	if err != nil {
		t.Fatalf("AssembleBucketRegistry: %v", err)
	}
	if reg == nil {
		t.Fatal("AssembleBucketRegistry returned nil")
	}
	if reg.MaxMultipartUploads("stored") != 0 {
		t.Error("stored bucket carried an unexpected multipart limit")
	}
}

// TestAssembleBucketRegistry_ReportsNotices verifies what the merge found
// reaches the caller that logs it, rather than being discarded.
func TestAssembleBucketRegistry_ReportsNotices(t *testing.T) {
	t.Parallel()

	store := storetest.NewMockProvisioningStore(gomock.NewController(t))
	a := gomock.Any()
	store.EXPECT().ListBuckets(a).Return([]core.Bucket{{Name: "photos"}}, nil).AnyTimes()
	store.EXPECT().ListUsers(a).Return(nil, nil).AnyTimes()
	store.EXPECT().ListCredentials(a).Return(nil, nil).AnyTimes()
	store.EXPECT().ListGrants(a).
		Return([]core.Grant{{UserID: "ghost", BucketName: "nowhere"}}, nil).AnyTimes()

	inj := do.New()
	do.ProvideValue[core.ProvisioningStore](inj, store)

	// photos collides with the config bucket and the grant names nothing, so
	// assembly serves through both and reports each.
	if _, err := AssembleBucketRegistry(context.Background(), inj,
		&config.Config{Buckets: []config.BucketConfig{{Name: "photos"}}}); err != nil {
		t.Fatalf("AssembleBucketRegistry: %v", err)
	}
}

// TestAssembleBucketRegistry_StoreUnavailable verifies a store that cannot be
// read fails rather than falling back to config alone, which would answer 403 to
// callers holding stored credentials.
func TestAssembleBucketRegistry_StoreUnavailable(t *testing.T) {
	t.Parallel()

	if _, err := AssembleBucketRegistry(context.Background(), do.New(), &config.Config{}); err == nil {
		t.Fatal("assembly succeeded with no store registered")
	}
}

// TestAssembleBucketRegistry_RejectsAmbiguousConfig verifies a credential two
// config buckets both claim still fails assembly, so the store merge did not
// weaken the backstop that keeps it from becoming a cross-bucket grant.
func TestAssembleBucketRegistry_RejectsAmbiguousConfig(t *testing.T) {
	t.Parallel()

	inj := do.New()
	do.ProvideValue[core.ProvisioningStore](inj, emptyProvisioningStore(t))

	_, err := AssembleBucketRegistry(context.Background(), inj, &config.Config{
		Buckets: []config.BucketConfig{
			{Name: "a", Credentials: []config.CredentialConfig{{Token: "SAME"}}},
			{Name: "b", Credentials: []config.CredentialConfig{{Token: "SAME"}}},
		},
	})
	if err == nil {
		t.Fatal("assembly accepted a token claimed by two buckets")
	}
}

// TestReadProvisioned_ListingFailurePropagates verifies a failure reading any
// one table fails the whole read, so assembly never runs against a partial
// picture of what the store holds.
func TestReadProvisioned_ListingFailurePropagates(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	for _, tc := range []struct {
		name  string
		setup func(*storetest.MockProvisioningStore, gomock.Matcher)
	}{
		{"buckets", func(s *storetest.MockProvisioningStore, a gomock.Matcher) {
			s.EXPECT().ListBuckets(a).Return(nil, boom)
		}},
		{"users", func(s *storetest.MockProvisioningStore, a gomock.Matcher) {
			s.EXPECT().ListBuckets(a).Return(nil, nil)
			s.EXPECT().ListUsers(a).Return(nil, boom)
		}},
		{"credentials", func(s *storetest.MockProvisioningStore, a gomock.Matcher) {
			s.EXPECT().ListBuckets(a).Return(nil, nil)
			s.EXPECT().ListUsers(a).Return(nil, nil)
			s.EXPECT().ListCredentials(a).Return(nil, boom)
		}},
		{"grants", func(s *storetest.MockProvisioningStore, a gomock.Matcher) {
			s.EXPECT().ListBuckets(a).Return(nil, nil)
			s.EXPECT().ListUsers(a).Return(nil, nil)
			s.EXPECT().ListCredentials(a).Return(nil, nil)
			s.EXPECT().ListGrants(a).Return(nil, boom)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := storetest.NewMockProvisioningStore(gomock.NewController(t))
			tc.setup(s, gomock.Any())
			if _, err := readProvisioned(context.Background(), s); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want a wrap of boom", err)
			}
		})
	}
}

// -------------------------------------------------------------------------
// MERGE
// -------------------------------------------------------------------------

// TestMergeProvisioned_StoredBucketsFollowConfig verifies a stored bucket joins
// the merged list and carries its own settings across.
func TestMergeProvisioned_StoredBucketsFollowConfig(t *testing.T) {
	t.Parallel()

	merged := mergeProvisioned(
		[]config.BucketConfig{{Name: "from-config"}},
		&provisioned{Buckets: []core.Bucket{{Name: "from-store", MaxMultipartUploads: 7}}},
	)

	got := bucketNames(merged.Buckets)
	if len(got) != 2 || got[0] != "from-config" || got[1] != "from-store" {
		t.Fatalf("merged buckets = %v, want [from-config from-store]", got)
	}
	if merged.Buckets[1].MaxMultipartUploads != 7 {
		t.Errorf("stored bucket limit = %d, want 7", merged.Buckets[1].MaxMultipartUploads)
	}
	// A stored bucket's users reach it through grants, so it contributes no
	// credentials of its own and nothing should read the field.
	if len(merged.Buckets[1].Credentials) != 0 {
		t.Errorf("stored bucket carried %d credentials, want none", len(merged.Buckets[1].Credentials))
	}
}

// TestMergeProvisioned_ConfigWinsNameCollision verifies a stored bucket sharing
// a config bucket's name is dropped and reported, so an operator reading the
// config file can trust what it says.
func TestMergeProvisioned_ConfigWinsNameCollision(t *testing.T) {
	t.Parallel()

	merged := mergeProvisioned(
		[]config.BucketConfig{{Name: "photos", MaxMultipartUploads: 1}},
		&provisioned{Buckets: []core.Bucket{{Name: "photos", MaxMultipartUploads: 99}}},
	)

	if got := bucketNames(merged.Buckets); len(got) != 1 || got[0] != "photos" {
		t.Fatalf("merged buckets = %v, want [photos]", got)
	}
	if merged.Buckets[0].MaxMultipartUploads != 1 {
		t.Errorf("config bucket was overwritten by the stored one: limit = %d, want 1",
			merged.Buckets[0].MaxMultipartUploads)
	}
	if n := countNotices(merged.Notices, auth.NoticeCredentialShadowed); n != 1 {
		t.Errorf("shadowing notices = %d, want 1", n)
	}
}

// TestMergeProvisioned_GrantsJoinOntoUser verifies a user reaches every bucket
// it holds a grant on, including one the config file declares rather than the
// store.
func TestMergeProvisioned_GrantsJoinOntoUser(t *testing.T) {
	t.Parallel()

	merged := mergeProvisioned(
		[]config.BucketConfig{{Name: "config-bucket"}},
		&provisioned{
			Buckets: []core.Bucket{{Name: "store-bucket"}},
			Users:   []core.User{{ID: "u1", Name: "ci"}},
			Credentials: []core.Credential{
				{AccessKeyID: "AK1", UserID: "u1", Secret: "s1"},
			},
			Grants: []core.Grant{
				{UserID: "u1", BucketName: "store-bucket"},
				{UserID: "u1", BucketName: "config-bucket"},
			},
		},
	)

	cred, ok := findCredential(merged.Credentials, "AK1")
	if !ok {
		t.Fatal("AK1 missing from assembled credentials")
	}
	for _, want := range []string{"store-bucket", "config-bucket"} {
		if !cred.User.CanReach(want) {
			t.Errorf("user reaches %v, want it to include %q", cred.User.Buckets(), want)
		}
	}
	if cred.Secret != "s1" {
		t.Errorf("secret = %q, want s1", cred.Secret)
	}
}

// TestMergeProvisioned_DanglingGrantReported verifies a grant naming a bucket
// neither source declares is skipped and reported rather than failing assembly.
// A config bucket can be removed while a grant to it survives in the store.
func TestMergeProvisioned_DanglingGrantReported(t *testing.T) {
	t.Parallel()

	merged := mergeProvisioned(nil, &provisioned{
		Users:       []core.User{{ID: "u1", Name: "ci"}},
		Credentials: []core.Credential{{AccessKeyID: "AK1", UserID: "u1", Secret: "s1"}},
		Grants:      []core.Grant{{UserID: "u1", BucketName: "gone"}},
	})

	if n := countNotices(merged.Notices, auth.NoticeDanglingGrant); n != 1 {
		t.Fatalf("dangling-grant notices = %d, want 1", n)
	}
	cred, ok := findCredential(merged.Credentials, "AK1")
	if !ok {
		t.Fatal("AK1 missing: a dangling grant must not drop the credential")
	}
	if cred.User.CanReach("gone") {
		t.Error("user reaches a bucket nothing declares")
	}
}

// TestMergeProvisioned_DisabledCredentialOmitted verifies a disabled credential
// never reaches the registry, which is what makes disabling one take effect
// while its row survives for the record of what it did.
func TestMergeProvisioned_DisabledCredentialOmitted(t *testing.T) {
	t.Parallel()

	merged := mergeProvisioned(nil, &provisioned{
		Buckets: []core.Bucket{{Name: "b"}},
		Users:   []core.User{{ID: "u1", Name: "ci"}},
		Credentials: []core.Credential{
			{AccessKeyID: "LIVE", UserID: "u1", Secret: "s1"},
			{AccessKeyID: "OFF", UserID: "u1", Secret: "s2", Disabled: true},
		},
		Grants: []core.Grant{{UserID: "u1", BucketName: "b"}},
	})

	if _, ok := findCredential(merged.Credentials, "OFF"); ok {
		t.Error("a disabled credential reached the registry")
	}
	if _, ok := findCredential(merged.Credentials, "LIVE"); !ok {
		t.Error("disabling one credential removed its sibling")
	}
}

// TestMergeProvisioned_CredentialWithoutUserOmitted verifies a credential whose
// user is absent resolves to nothing rather than to a user with no identity.
func TestMergeProvisioned_CredentialWithoutUserOmitted(t *testing.T) {
	t.Parallel()

	merged := mergeProvisioned(nil, &provisioned{
		Credentials: []core.Credential{{AccessKeyID: "AK1", UserID: "missing", Secret: "s1"}},
	})

	if len(merged.Credentials) != 0 {
		t.Errorf("assembled %d credentials, want none", len(merged.Credentials))
	}
}

// TestMergeProvisioned_UserWithoutGrantsReachesNothing verifies a user holding
// no grants still authenticates and is refused on every bucket, which is a
// clearer answer than a failed signature for a user granted nothing yet.
func TestMergeProvisioned_UserWithoutGrantsReachesNothing(t *testing.T) {
	t.Parallel()

	merged := mergeProvisioned(
		[]config.BucketConfig{{Name: "photos"}},
		&provisioned{
			Users:       []core.User{{ID: "u1", Name: "new"}},
			Credentials: []core.Credential{{AccessKeyID: "AK1", UserID: "u1", Secret: "s1"}},
		},
	)

	cred, ok := findCredential(merged.Credentials, "AK1")
	if !ok {
		t.Fatal("AK1 missing from assembled credentials")
	}
	if len(cred.User.Buckets()) != 0 {
		t.Errorf("user reaches %v, want nothing", cred.User.Buckets())
	}
	if cred.User.CanReach("photos") {
		t.Error("user with no grants reaches a bucket")
	}
}
