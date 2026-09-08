// -------------------------------------------------------------------------------
// Ops - Provisioning Tests
//
// Author: Alex Freidah
//
// Covers what each operation refuses and what it writes: config-declared
// entries staying read-only, a bucket that still holds objects or grants
// staying undeletable, a user that still holds either staying undeletable, a
// minted keypair returning its secret exactly once, and every mutation
// rebuilding the registry before it reports success.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// HARNESS
// -------------------------------------------------------------------------

// provFixture is a Provisioning service over mocked collaborators, with the
// store's four listings already stated.
type provFixture struct {
	svc      *Provisioning
	store    *opstest.MockProvisioningStore
	objects  *opstest.MockNamespaceCounter
	registry *opstest.MockRegistryPublisher
}

// provStore is what the store answers its listings with.
type provStore struct {
	buckets     []core.Bucket
	users       []core.User
	credentials []core.Credential
	grants      []core.Grant
}

// newProvFixture builds the service over a store holding rows and a config
// declaring buckets.
func newProvFixture(t *testing.T, cfgBuckets []config.BucketConfig, rows *provStore) *provFixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	f := &provFixture{
		store:    opstest.NewMockProvisioningStore(ctrl),
		objects:  opstest.NewMockNamespaceCounter(ctrl),
		registry: opstest.NewMockRegistryPublisher(ctrl),
	}
	a := gomock.Any()
	f.store.EXPECT().ListBuckets(a).Return(rows.buckets, nil).AnyTimes()
	f.store.EXPECT().ListUsers(a).Return(rows.users, nil).AnyTimes()
	f.store.EXPECT().ListCredentials(a).Return(rows.credentials, nil).AnyTimes()
	f.store.EXPECT().ListGrants(a).Return(rows.grants, nil).AnyTimes()

	f.svc = NewProvisioning(ProvisioningDeps{
		Store:    f.store,
		Objects:  f.objects,
		Registry: f.registry,
		Config:   NewConfigStore(&config.Config{Buckets: cfgBuckets}),
	})
	return f
}

// declaredBuckets builds the live bucket set an operation resolves a key
// against, holding the named buckets.
func declaredBuckets(names ...string) *provisioning.Declared {
	buckets := make([]provisioning.Bucket, 0, len(names))
	for _, n := range names {
		buckets = append(buckets, provisioning.Bucket{Name: n, Source: provisioning.SourceStore})
	}
	d := provisioning.NewDeclared()
	d.Set(buckets)
	return d
}

// expectRepublish states that the operation under test must rebuild the
// registry, which is what makes a change take effect on the next request.
func (f *provFixture) expectRepublish() {
	f.registry.EXPECT().Republish(gomock.Any()).Return(nil)
}

// -------------------------------------------------------------------------
// VIEW
// -------------------------------------------------------------------------

// TestProvisioning_ViewMergesBothSources verifies the listing reports what each
// source declares, each entry saying where it came from.
func TestProvisioning_ViewMergesBothSources(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t,
		[]config.BucketConfig{{Name: "from-config"}},
		&provStore{buckets: []core.Bucket{{Name: "from-store"}}})

	v, err := f.svc.View(context.Background())
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if len(v.Buckets) != 2 {
		t.Fatalf("buckets = %d, want both sources", len(v.Buckets))
	}
}

// TestProvisioning_ViewReadFailurePropagates verifies a store that cannot be
// read fails rather than reporting config alone as the whole picture.
func TestProvisioning_ViewReadFailurePropagates(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	ctrl := gomock.NewController(t)
	store := opstest.NewMockProvisioningStore(ctrl)
	store.EXPECT().ListBuckets(gomock.Any()).Return(nil, boom)

	svc := NewProvisioning(ProvisioningDeps{Store: store, Config: NewConfigStore(&config.Config{})})
	if _, err := svc.View(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want a wrap of boom", err)
	}
}

// -------------------------------------------------------------------------
// BUCKETS
// -------------------------------------------------------------------------

// TestProvisioning_CreateBucket verifies a declared bucket is written and the
// registry rebuilt.
func TestProvisioning_CreateBucket(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	f.store.EXPECT().CreateBucket(gomock.Any(), gomock.Any()).Return(nil)
	f.expectRepublish()

	if err := f.svc.CreateBucket(context.Background(), &core.Bucket{Name: "photos"}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
}

// TestProvisioning_CreateBucketRejectsEmptyName verifies a bucket with no name
// never reaches the store.
func TestProvisioning_CreateBucketRejectsEmptyName(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	if err := f.svc.CreateBucket(context.Background(), &core.Bucket{}); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("err = %v, want ErrNameRequired", err)
	}
}

// TestProvisioning_CreateBucketRejectsExisting verifies a name either source
// already declares is refused rather than duplicated.
func TestProvisioning_CreateBucketRejectsExisting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  []config.BucketConfig
		rows provStore
	}{
		{"config", []config.BucketConfig{{Name: "photos"}}, provStore{}},
		{"store", nil, provStore{buckets: []core.Bucket{{Name: "photos"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProvFixture(t, tc.cfg, &tc.rows)
			err := f.svc.CreateBucket(context.Background(), &core.Bucket{Name: "photos"})
			if !errors.Is(err, ErrBucketExists) {
				t.Fatalf("err = %v, want ErrBucketExists", err)
			}
		})
	}
}

// TestProvisioning_DeleteBucket verifies an empty stored bucket is removed and
// the registry rebuilt.
func TestProvisioning_DeleteBucket(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{buckets: []core.Bucket{{Name: "photos"}}})
	f.objects.EXPECT().CountObjectsByPrefix(gomock.Any(), "photos/").Return(int64(0), nil)
	f.store.EXPECT().DeleteBucket(gomock.Any(), "photos").Return(nil)
	f.expectRepublish()

	if err := f.svc.DeleteBucket(context.Background(), "photos"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
}

// TestProvisioning_DeleteBucketRejectsUnknown verifies a name nothing declares
// is reported as missing rather than silently succeeding.
func TestProvisioning_DeleteBucketRejectsUnknown(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	if err := f.svc.DeleteBucket(context.Background(), "gone"); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("err = %v, want ErrBucketNotFound", err)
	}
}

// TestProvisioning_DeleteBucketRejectsConfigDeclared verifies the API refuses to
// remove something the config file declares, so an operator reading that file
// can trust what it says.
func TestProvisioning_DeleteBucketRejectsConfigDeclared(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, []config.BucketConfig{{Name: "photos"}}, &provStore{})
	if err := f.svc.DeleteBucket(context.Background(), "photos"); !errors.Is(err, ErrConfigDeclared) {
		t.Fatalf("err = %v, want ErrConfigDeclared", err)
	}
}

// TestProvisioning_DeleteBucketRejectsNonEmpty verifies a bucket still holding
// objects stays declared: dropping it would leave those keys addressable by
// nothing while still occupying every backend they were written to.
func TestProvisioning_DeleteBucketRejectsNonEmpty(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{buckets: []core.Bucket{{Name: "photos"}}})
	f.objects.EXPECT().CountObjectsByPrefix(gomock.Any(), "photos/").Return(int64(12), nil)

	err := f.svc.DeleteBucket(context.Background(), "photos")
	if !errors.Is(err, ErrBucketNotEmpty) {
		t.Fatalf("err = %v, want ErrBucketNotEmpty", err)
	}
}

// TestProvisioning_DeleteBucketCountFailurePropagates verifies a count that
// cannot be taken refuses the delete rather than assuming the bucket is empty.
func TestProvisioning_DeleteBucketCountFailurePropagates(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	f := newProvFixture(t, nil, &provStore{buckets: []core.Bucket{{Name: "photos"}}})
	f.objects.EXPECT().CountObjectsByPrefix(gomock.Any(), "photos/").Return(int64(0), boom)

	if err := f.svc.DeleteBucket(context.Background(), "photos"); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want a wrap of boom", err)
	}
}

// TestProvisioning_DeleteBucketRejectsGranted verifies a bucket a user still
// reaches stays declared, so the grant does not become a dangling one reported
// on every assembly.
func TestProvisioning_DeleteBucketRejectsGranted(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{
		buckets: []core.Bucket{{Name: "photos"}},
		users:   []core.User{{ID: "u1", Name: "ci"}},
		grants:  []core.Grant{{UserID: "u1", BucketName: "photos"}},
	})
	f.objects.EXPECT().CountObjectsByPrefix(gomock.Any(), "photos/").Return(int64(0), nil)

	if err := f.svc.DeleteBucket(context.Background(), "photos"); !errors.Is(err, ErrBucketGranted) {
		t.Fatalf("err = %v, want ErrBucketGranted", err)
	}
}

// -------------------------------------------------------------------------
// USERS
// -------------------------------------------------------------------------

// TestProvisioning_CreateUser verifies a user is written with a generated id
// and the registry rebuilt.
func TestProvisioning_CreateUser(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	f.store.EXPECT().CreateUser(gomock.Any(), gomock.Any()).Return(nil)
	f.expectRepublish()

	u, err := f.svc.CreateUser(context.Background(), "ci")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.ID == "" {
		t.Error("created user carries no id")
	}
	if u.Name != "ci" {
		t.Errorf("name = %q, want ci", u.Name)
	}
}

// TestProvisioning_CreateUserRejectsEmptyName verifies an unnamed user never
// reaches the store.
func TestProvisioning_CreateUserRejectsEmptyName(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	if _, err := f.svc.CreateUser(context.Background(), ""); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("err = %v, want ErrNameRequired", err)
	}
}

// TestProvisioning_DeleteUser verifies a user holding nothing is removed.
func TestProvisioning_DeleteUser(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{users: []core.User{{ID: "u1", Name: "ci"}}})
	f.store.EXPECT().DeleteUser(gomock.Any(), "u1").Return(nil)
	f.expectRepublish()

	if err := f.svc.DeleteUser(context.Background(), "u1"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
}

// TestProvisioning_DeleteUserRejectsUnknown verifies an id nothing declares is
// reported as missing.
func TestProvisioning_DeleteUserRejectsUnknown(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	if err := f.svc.DeleteUser(context.Background(), "u1"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

// TestProvisioning_DeleteUserRejectsConfigDeclared verifies a user the config
// file implies is read-only through the API.
func TestProvisioning_DeleteUserRejectsConfigDeclared(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, []config.BucketConfig{
		{Name: "photos", Credentials: []config.CredentialConfig{{AccessKeyID: "AK", SecretAccessKey: "SK"}}},
	}, &provStore{})

	if err := f.svc.DeleteUser(context.Background(), "config:AK"); !errors.Is(err, ErrConfigDeclared) {
		t.Fatalf("err = %v, want ErrConfigDeclared", err)
	}
}

// TestProvisioning_DeleteUserRejectsInUse verifies a user is told which of its
// two kinds of holding is in the way, rather than a foreign-key violation.
func TestProvisioning_DeleteUserRejectsInUse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rows provStore
	}{
		{"grants", provStore{
			buckets: []core.Bucket{{Name: "photos"}},
			users:   []core.User{{ID: "u1", Name: "ci"}},
			grants:  []core.Grant{{UserID: "u1", BucketName: "photos"}},
		}},
		{"credentials", provStore{
			users:       []core.User{{ID: "u1", Name: "ci"}},
			credentials: []core.Credential{{AccessKeyID: "AK", UserID: "u1", Secret: "s"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProvFixture(t, nil, &tc.rows)
			if err := f.svc.DeleteUser(context.Background(), "u1"); !errors.Is(err, ErrUserInUse) {
				t.Fatalf("err = %v, want ErrUserInUse", err)
			}
		})
	}
}

// -------------------------------------------------------------------------
// CREDENTIALS
// -------------------------------------------------------------------------

// TestProvisioning_CreateCredential verifies a keypair is minted for an
// existing user, returned whole, and stored with the same material.
func TestProvisioning_CreateCredential(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{users: []core.User{{ID: "u1", Name: "ci"}}})
	var stored core.Credential
	f.store.EXPECT().CreateCredential(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, c *core.Credential) error {
			stored = *c
			return nil
		})
	f.expectRepublish()

	got, err := f.svc.CreateCredential(context.Background(), "u1", "deploy job")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if got.AccessKeyID == "" || got.Secret == "" {
		t.Fatalf("minted credential = %+v, want both halves", got)
	}
	if stored.AccessKeyID != got.AccessKeyID || stored.Secret != got.Secret {
		t.Error("the stored keypair is not the one returned to the caller")
	}
	if stored.UserID != "u1" || stored.Label != "deploy job" {
		t.Errorf("stored credential = %+v, want it to carry the user and label", stored)
	}
}

// TestProvisioning_CreateCredentialIsUnique verifies two mints do not collide,
// so issuing one credential cannot overwrite another.
func TestProvisioning_CreateCredentialIsUnique(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{users: []core.User{{ID: "u1", Name: "ci"}}})
	f.store.EXPECT().CreateCredential(gomock.Any(), gomock.Any()).Return(nil).Times(2)
	f.registry.EXPECT().Republish(gomock.Any()).Return(nil).Times(2)

	first, err := f.svc.CreateCredential(context.Background(), "u1", "")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	second, err := f.svc.CreateCredential(context.Background(), "u1", "")
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if first.AccessKeyID == second.AccessKeyID || first.Secret == second.Secret {
		t.Error("two mints produced the same material")
	}
}

// TestProvisioning_CreateCredentialRequiresUser verifies a keypair with no owner
// is refused: one nobody can account for is worse than none.
func TestProvisioning_CreateCredentialRequiresUser(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	if _, err := f.svc.CreateCredential(context.Background(), "", ""); !errors.Is(err, ErrUserRequired) {
		t.Fatalf("err = %v, want ErrUserRequired", err)
	}
}

// TestProvisioning_CreateCredentialRejectsUnknownUser verifies a credential is
// never minted against an identity nothing declares.
func TestProvisioning_CreateCredentialRejectsUnknownUser(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	if _, err := f.svc.CreateCredential(context.Background(), "u1", ""); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

// TestProvisioning_CreateCredentialRejectsConfigUser verifies the API refuses to
// widen what a config-declared credential reaches by hanging a second keypair
// off its synthesised identity.
func TestProvisioning_CreateCredentialRejectsConfigUser(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, []config.BucketConfig{
		{Name: "photos", Credentials: []config.CredentialConfig{{AccessKeyID: "AK", SecretAccessKey: "SK"}}},
	}, &provStore{})

	_, err := f.svc.CreateCredential(context.Background(), "config:AK", "")
	if !errors.Is(err, ErrConfigDeclared) {
		t.Fatalf("err = %v, want ErrConfigDeclared", err)
	}
}

// TestProvisioning_DeleteCredential verifies one keypair is revoked and the
// registry rebuilt, so revocation takes effect on the next request.
func TestProvisioning_DeleteCredential(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{
		users:       []core.User{{ID: "u1", Name: "ci"}},
		credentials: []core.Credential{{AccessKeyID: "AK", UserID: "u1", Secret: "s"}},
	})
	f.store.EXPECT().DeleteCredential(gomock.Any(), "AK").Return(nil)
	f.expectRepublish()

	if err := f.svc.DeleteCredential(context.Background(), "AK"); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
}

// TestProvisioning_DeleteCredentialRejectsUnknown verifies an access key nothing
// declares is reported as missing.
func TestProvisioning_DeleteCredentialRejectsUnknown(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	err := f.svc.DeleteCredential(context.Background(), "AK")
	if !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("err = %v, want ErrCredentialNotFound", err)
	}
}

// TestProvisioning_DeleteCredentialRejectsConfigDeclared verifies a keypair the
// config file declares is removed by editing that file, not through the API.
func TestProvisioning_DeleteCredentialRejectsConfigDeclared(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, []config.BucketConfig{
		{Name: "photos", Credentials: []config.CredentialConfig{{AccessKeyID: "AK", SecretAccessKey: "SK"}}},
	}, &provStore{})

	if err := f.svc.DeleteCredential(context.Background(), "AK"); !errors.Is(err, ErrConfigDeclared) {
		t.Fatalf("err = %v, want ErrConfigDeclared", err)
	}
}

// -------------------------------------------------------------------------
// GRANTS
// -------------------------------------------------------------------------

// TestProvisioning_CreateGrant verifies a stored user can be granted a bucket
// from either source, which is how a client is onboarded onto one the
// deployment already runs.
func TestProvisioning_CreateGrant(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		cfg    []config.BucketConfig
		stored []core.Bucket
	}{
		{"config bucket", []config.BucketConfig{{Name: "photos"}}, nil},
		{"stored bucket", nil, []core.Bucket{{Name: "photos"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProvFixture(t, tc.cfg, &provStore{
				buckets: tc.stored,
				users:   []core.User{{ID: "u1", Name: "ci"}},
			})
			f.store.EXPECT().CreateGrant(gomock.Any(), gomock.Any()).Return(nil)
			f.expectRepublish()

			if err := f.svc.CreateGrant(context.Background(), "u1", "photos"); err != nil {
				t.Fatalf("CreateGrant: %v", err)
			}
		})
	}
}

// TestProvisioning_CreateGrantRejectsUnknown verifies a grant naming an
// identity or a bucket nothing declares never reaches the store, so a dangling
// one is not created deliberately.
func TestProvisioning_CreateGrantRejectsUnknown(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rows provStore
		want error
	}{
		{"user", provStore{buckets: []core.Bucket{{Name: "photos"}}}, ErrUserNotFound},
		{"bucket", provStore{users: []core.User{{ID: "u1", Name: "ci"}}}, ErrBucketNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProvFixture(t, nil, &tc.rows)
			if err := f.svc.CreateGrant(context.Background(), "u1", "photos"); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestProvisioning_DeleteGrant verifies one grant is withdrawn and the registry
// rebuilt.
func TestProvisioning_DeleteGrant(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{
		buckets: []core.Bucket{{Name: "photos"}},
		users:   []core.User{{ID: "u1", Name: "ci"}},
		grants:  []core.Grant{{UserID: "u1", BucketName: "photos"}},
	})
	f.store.EXPECT().DeleteGrant(gomock.Any(), "u1", "photos").Return(nil)
	f.expectRepublish()

	if err := f.svc.DeleteGrant(context.Background(), "u1", "photos"); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
}

// TestProvisioning_DeleteGrantRejectsConfigUser verifies what a config-declared
// credential reaches cannot be narrowed through the API.
func TestProvisioning_DeleteGrantRejectsConfigUser(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, []config.BucketConfig{
		{Name: "photos", Credentials: []config.CredentialConfig{{AccessKeyID: "AK", SecretAccessKey: "SK"}}},
	}, &provStore{})

	err := f.svc.DeleteGrant(context.Background(), "config:AK", "photos")
	if !errors.Is(err, ErrConfigDeclared) {
		t.Fatalf("err = %v, want ErrConfigDeclared", err)
	}
}

// TestProvisioning_DeleteGrantRejectsUnknownUser verifies an identity nothing
// declares is reported as missing.
func TestProvisioning_DeleteGrantRejectsUnknownUser(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	if err := f.svc.DeleteGrant(context.Background(), "u1", "photos"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

// -------------------------------------------------------------------------
// REPUBLISH
// -------------------------------------------------------------------------

// TestProvisioning_RepublishFailureIsReported verifies a write that lands
// without the registry rebuilding is reported as an error: a caller told the
// write succeeded would go on to use a credential that authenticates nothing.
func TestProvisioning_RepublishFailureIsReported(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	f := newProvFixture(t, nil, &provStore{})
	f.store.EXPECT().CreateBucket(gomock.Any(), gomock.Any()).Return(nil)
	f.registry.EXPECT().Republish(gomock.Any()).Return(boom)

	err := f.svc.CreateBucket(context.Background(), &core.Bucket{Name: "photos"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want a wrap of boom", err)
	}
}

// TestProvisioning_WriteFailurePropagates verifies a store rejection reaches the
// caller rather than being reported as a successful change.
func TestProvisioning_WriteFailurePropagates(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	f := newProvFixture(t, nil, &provStore{})
	f.store.EXPECT().CreateBucket(gomock.Any(), gomock.Any()).Return(boom)

	err := f.svc.CreateBucket(context.Background(), &core.Bucket{Name: "photos"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want a wrap of boom", err)
	}
}

// TestProvisioning_NoPublisherIsTolerated verifies a service wired without a
// publisher still writes, which is what lets an operation run in a deployment
// with no serving transport attached.
func TestProvisioning_NoPublisherIsTolerated(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	store := opstest.NewMockProvisioningStore(ctrl)
	a := gomock.Any()
	store.EXPECT().ListBuckets(a).Return(nil, nil)
	store.EXPECT().ListUsers(a).Return(nil, nil)
	store.EXPECT().ListCredentials(a).Return(nil, nil)
	store.EXPECT().ListGrants(a).Return(nil, nil)
	store.EXPECT().CreateBucket(a, a).Return(nil)

	svc := NewProvisioning(ProvisioningDeps{Store: store, Config: NewConfigStore(&config.Config{})})
	if err := svc.CreateBucket(context.Background(), &core.Bucket{Name: "photos"}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
}
