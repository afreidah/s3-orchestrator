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
	"strings"
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
	return newProvFixtureCfg(t, &config.Config{Buckets: cfgBuckets}, rows)
}

// newProvFixtureCfg is newProvFixture over a whole config, which the grants
// naming a backend need: which backends exist comes from config rather than
// from the store.
func newProvFixtureCfg(t *testing.T, cfg *config.Config, rows *provStore) *provFixture {
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
		Config:   NewConfigStore(cfg),
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
		grants:  []core.Grant{{UserID: "u1", Resource: core.BucketResource("photos")}},
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
			grants:  []core.Grant{{UserID: "u1", Resource: core.BucketResource("photos")}},
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

	got, err := f.svc.CreateCredential(context.Background(), "u1", "deploy job", Keypair{})
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

	first, err := f.svc.CreateCredential(context.Background(), "u1", "", Keypair{})
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	second, err := f.svc.CreateCredential(context.Background(), "u1", "", Keypair{})
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
	if _, err := f.svc.CreateCredential(context.Background(), "", "", Keypair{}); !errors.Is(err, ErrUserRequired) {
		t.Fatalf("err = %v, want ErrUserRequired", err)
	}
}

// TestProvisioning_CreateCredentialRejectsUnknownUser verifies a credential is
// never minted against an identity nothing declares.
func TestProvisioning_CreateCredentialRejectsUnknownUser(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	if _, err := f.svc.CreateCredential(context.Background(), "u1", "", Keypair{}); !errors.Is(err, ErrUserNotFound) {
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

	_, err := f.svc.CreateCredential(context.Background(), "config:AK", "", Keypair{})
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

			if err := f.svc.CreateGrant(context.Background(), "u1", core.BucketResource("photos"), core.PermAll); err != nil {
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
			if err := f.svc.CreateGrant(context.Background(), "u1", core.BucketResource("photos"), core.PermAll); !errors.Is(err, tc.want) {
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
		grants:  []core.Grant{{UserID: "u1", Resource: core.BucketResource("photos")}},
	})
	f.store.EXPECT().DeleteGrant(gomock.Any(), "u1", core.BucketResource("photos")).Return(nil)
	f.expectRepublish()

	if err := f.svc.DeleteGrant(context.Background(), "u1", core.BucketResource("photos")); err != nil {
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

	err := f.svc.DeleteGrant(context.Background(), "config:AK", core.BucketResource("photos"))
	if !errors.Is(err, ErrConfigDeclared) {
		t.Fatalf("err = %v, want ErrConfigDeclared", err)
	}
}

// TestProvisioning_DeleteGrantRejectsUnknownUser verifies an identity nothing
// declares is reported as missing.
func TestProvisioning_DeleteGrantRejectsUnknownUser(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{})
	if err := f.svc.DeleteGrant(context.Background(), "u1", core.BucketResource("photos")); !errors.Is(err, ErrUserNotFound) {
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

// -------------------------------------------------------------------------
// MESSAGES
// -------------------------------------------------------------------------

// TestPlural verifies a refusal naming one thing does not read as though it
// named several. This is the text an operator sees at the moment they are told
// no, so it is worth getting right.
func TestPlural(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		n    int64
		noun string
		want string
	}{
		{0, "object", "0 objects"},
		{1, "object", "1 object"},
		{2, "object", "2 objects"},
		{1, "grant", "1 grant"},
		{3, "credential", "3 credentials"},
	} {
		if got := plural(tc.n, tc.noun); got != tc.want {
			t.Errorf("plural(%d, %q) = %q, want %q", tc.n, tc.noun, got, tc.want)
		}
	}
}

// TestProvisioning_RefusalsReadNaturally drives the three refusals that carry a
// count and checks the one-of-each case, which is the wording the old messages
// got wrong.
func TestProvisioning_RefusalsReadNaturally(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{buckets: []core.Bucket{{Name: "photos"}}})
	f.objects.EXPECT().CountObjectsByPrefix(gomock.Any(), "photos/").Return(int64(1), nil)

	err := f.svc.DeleteBucket(context.Background(), "photos")
	if err == nil || !strings.Contains(err.Error(), "holds 1 object") {
		t.Fatalf("err = %v, want it to name one object in the singular", err)
	}
	if strings.Contains(err.Error(), "1 objects") {
		t.Errorf("err = %v, still pluralises a count of one", err)
	}
}

// -------------------------------------------------------------------------
// GRANT PERMISSIONS
// -------------------------------------------------------------------------

// TestProvisioning_CreateGrantStoresPermissions verifies the set the caller
// asked for is what lands, rather than being widened to full access.
func TestProvisioning_CreateGrantStoresPermissions(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, []config.BucketConfig{{Name: "photos"}},
		&provStore{users: []core.User{{ID: "u1", Name: "ci"}}})

	var stored core.Grant
	f.store.EXPECT().CreateGrant(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, g *core.Grant) error {
			stored = *g
			return nil
		})
	f.expectRepublish()

	readOnly := core.PermListBuckets | core.PermList | core.PermRead
	if err := f.svc.CreateGrant(context.Background(), "u1", core.BucketResource("photos"), readOnly); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	if stored.Permissions != readOnly {
		t.Errorf("stored permissions = %q, want %q", stored.Permissions, readOnly)
	}
	if stored.Permissions.Has(core.PermDelete) {
		t.Error("a read-only grant was stored carrying delete")
	}
}

// TestProvisioning_CreateGrantRejectsEmptyPermissions verifies a grant carrying
// nothing is refused. It would reach the bucket and be denied every operation,
// which is a grant that does nothing but look like one.
func TestProvisioning_CreateGrantRejectsEmptyPermissions(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, []config.BucketConfig{{Name: "photos"}},
		&provStore{users: []core.User{{ID: "u1", Name: "ci"}}})

	err := f.svc.CreateGrant(context.Background(), "u1", core.BucketResource("photos"), 0)
	if !errors.Is(err, ErrNoPermissions) {
		t.Fatalf("err = %v, want ErrNoPermissions", err)
	}
}

// TestProvisioning_CreateGrantOnControlPlaneResources verifies which resources
// a grant may name, and that a wildcard is written without being looked up:
// naming things that do not exist yet is the point of it.
func TestProvisioning_CreateGrantOnControlPlaneResources(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Buckets:  []config.BucketConfig{{Name: "photos"}},
		Backends: []config.BackendConfig{{Name: "b1"}},
	}
	for _, tc := range []struct {
		name     string
		resource core.Resource
		perms    core.PermissionSet
		want     error
	}{
		{"a configured backend", core.Resource{Kind: core.ResourceBackend, Name: "b1"}, core.PermAdminDrain, nil},
		{"the backend wildcard", core.Resource{Kind: core.ResourceBackend, Name: core.ResourceWildcard}, core.PermAdminDrain, nil},
		{"the bucket wildcard", core.BucketResource(core.ResourceWildcard), core.PermAll, nil},
		{"the instance", core.Resource{Kind: core.ResourceOrchestrator}, core.PermAdminProvision, nil},
		{"a backend nothing serves", core.Resource{Kind: core.ResourceBackend, Name: "b9"}, core.PermAdminDrain, ErrBackendNotFound},
		{"an instance with a name", core.Resource{Kind: core.ResourceOrchestrator, Name: "b1"}, core.PermAdminProvision, ErrInvalidResource},
		{"a kind nothing knows", core.Resource{Kind: "region", Name: "us"}, core.PermAdminRead, ErrInvalidResource},
		{"admin permissions on a bucket", core.BucketResource("photos"), core.PermAdminDrain, ErrInvalidResource},
		{"data-plane permissions on a backend", core.Resource{Kind: core.ResourceBackend, Name: "b1"}, core.PermRead, ErrInvalidResource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProvFixtureCfg(t, cfg, &provStore{users: []core.User{{ID: "u1", Name: "ci"}}})
			if tc.want == nil {
				f.store.EXPECT().CreateGrant(gomock.Any(), gomock.Any()).Return(nil)
				f.expectRepublish()
			}
			err := f.svc.CreateGrant(context.Background(), "u1", tc.resource, tc.perms)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestProvisioning_ViewCarriesGrantPermissions verifies what a grant carries
// survives the merge into the view the registry is built from.
func TestProvisioning_ViewCarriesGrantPermissions(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{
		buckets: []core.Bucket{{Name: "photos"}},
		users:   []core.User{{ID: "u1", Name: "ci"}},
		grants: []core.Grant{{
			UserID:      "u1",
			Resource:    core.BucketResource("photos"),
			Permissions: core.PermRead | core.PermList,
		}},
	})

	v, err := f.svc.View(context.Background())
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	u, ok := findUser(v.Users, "u1")
	if !ok {
		t.Fatal("u1 missing from the merged users")
	}
	if got := u.Grants["photos"]; got != core.PermRead|core.PermList {
		t.Errorf("merged permissions = %q, want read,list", got)
	}
}

// TestProvisioning_WildcardReachDoesNotPinABucket verifies a bucket stays
// deletable when the only identity reaching it does so through a wildcard.
//
// What makes a bucket undeletable is a grant row that would be left naming
// nothing. A wildcard does not dangle, and the root credential holds one over
// every bucket, so judging this by the merged reach would make every bucket in
// a deployment declaring a root credential permanently undeletable.
func TestProvisioning_WildcardReachDoesNotPinABucket(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Auth: config.AuthConfig{
			Root: config.RootCredential{AccessKeyID: "AK", SecretAccessKey: "SK"},
		},
	}
	f := newProvFixtureCfg(t, cfg, &provStore{
		buckets: []core.Bucket{{Name: "transient"}},
		users:   []core.User{{ID: "u1", Name: "ci"}},
		grants: []core.Grant{{
			UserID:      "u1",
			Resource:    core.BucketResource(core.ResourceWildcard),
			Permissions: core.PermAll,
		}},
	})
	f.objects.EXPECT().CountObjectsByPrefix(gomock.Any(), "transient/").Return(int64(0), nil)
	f.store.EXPECT().DeleteBucket(gomock.Any(), "transient").Return(nil)
	f.expectRepublish()

	if err := f.svc.DeleteBucket(context.Background(), "transient"); err != nil {
		t.Fatalf("a bucket reached only by a wildcard was refused: %v", err)
	}
}

// TestProvisioning_NamedGrantStillPinsABucket pins the other side: a grant row
// naming the bucket still refuses the delete, because that row would be left
// pointing at nothing.
func TestProvisioning_NamedGrantStillPinsABucket(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{
		buckets: []core.Bucket{{Name: "photos"}},
		users:   []core.User{{ID: "u1", Name: "ci"}},
		grants: []core.Grant{{
			UserID:      "u1",
			Resource:    core.BucketResource("photos"),
			Permissions: core.PermAll,
		}},
	})
	f.objects.EXPECT().CountObjectsByPrefix(gomock.Any(), "photos/").Return(int64(0), nil)

	if err := f.svc.DeleteBucket(context.Background(), "photos"); !errors.Is(err, ErrBucketGranted) {
		t.Fatalf("err = %v, want ErrBucketGranted", err)
	}
}

// TestProvisioning_CreateCredentialRegistersASuppliedKeypair verifies a caller
// that already holds a keypair records that one rather than receiving a second.
// This is what lets a declarative caller converge: it re-registers what its
// secret store holds instead of rotating every client it manages.
func TestProvisioning_CreateCredentialRegistersASuppliedKeypair(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{users: []core.User{{ID: "u1", Name: "ci"}}})
	var stored core.Credential
	f.store.EXPECT().CreateCredential(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, c *core.Credential) error {
			stored = *c
			return nil
		})
	f.expectRepublish()

	supplied := Keypair{AccessKeyID: "AKIASUPPLIED", SecretAccessKey: "supplied-secret"}
	got, err := f.svc.CreateCredential(context.Background(), "u1", "vault-managed", supplied)
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if stored.AccessKeyID != supplied.AccessKeyID || stored.Secret != supplied.SecretAccessKey {
		t.Errorf("stored = %+v, want the supplied keypair rather than a minted one", stored)
	}
	// Echoed rather than withheld, so one response shape covers both paths and
	// the caller can confirm what was recorded against what it sent.
	if got.AccessKeyID != supplied.AccessKeyID || got.Secret != supplied.SecretAccessKey {
		t.Errorf("returned = %+v, want the supplied keypair echoed back", got)
	}
}

// TestProvisioning_CreateCredentialRejectsHalfAKeypair verifies one half alone
// is refused rather than quietly minting the other. A key with no secret cannot
// sign and a secret with no key names nothing, so either alone is a mistake.
func TestProvisioning_CreateCredentialRejectsHalfAKeypair(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		keys Keypair
	}{
		{"an access key with no secret", Keypair{AccessKeyID: "AKIAONLY"}},
		{"a secret with no access key", Keypair{SecretAccessKey: "secret-only"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProvFixture(t, nil, &provStore{users: []core.User{{ID: "u1", Name: "ci"}}})
			_, err := f.svc.CreateCredential(context.Background(), "u1", "", tc.keys)
			if !errors.Is(err, ErrKeypairIncomplete) {
				t.Fatalf("err = %v, want ErrKeypairIncomplete", err)
			}
		})
	}
}

// TestProvisioning_CreateCredentialRejectsAClaimedAccessKey verifies an access
// key another credential already holds is refused before the insert.
//
// Assembly refuses a key claimed twice, so without this check the row would
// land and the republish behind it would fail, leaving a credential nothing can
// authenticate.
func TestProvisioning_CreateCredentialRejectsAClaimedAccessKey(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		buckets []config.BucketConfig
		store   *provStore
		key     string
	}{
		{
			name: "claimed by a stored credential",
			store: &provStore{
				users:       []core.User{{ID: "u1", Name: "ci"}},
				credentials: []core.Credential{{AccessKeyID: "AKIATAKEN", UserID: "u1", Secret: "s"}},
			},
			key: "AKIATAKEN",
		},
		{
			// The store holds no row for a config credential, so its unique
			// constraint would not catch this one.
			name: "claimed by a config credential",
			buckets: []config.BucketConfig{
				{Name: "photos", Credentials: []config.CredentialConfig{
					{AccessKeyID: "AKIATAKEN", SecretAccessKey: "SK"},
				}},
			},
			store: &provStore{users: []core.User{{ID: "u1", Name: "ci"}}},
			key:   "AKIATAKEN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProvFixture(t, tc.buckets, tc.store)
			_, err := f.svc.CreateCredential(context.Background(), "u1", "",
				Keypair{AccessKeyID: tc.key, SecretAccessKey: "whatever"})
			if !errors.Is(err, ErrCredentialExists) {
				t.Fatalf("err = %v, want ErrCredentialExists", err)
			}
		})
	}
}

// TestProvisioning_RenameUser verifies the name changes and the id does not,
// so credentials and grants hanging off the user keep resolving.
func TestProvisioning_RenameUser(t *testing.T) {
	t.Parallel()

	f := newProvFixture(t, nil, &provStore{users: []core.User{{ID: "u1", Name: "old"}}})
	f.store.EXPECT().RenameUser(gomock.Any(), "u1", "new").Return(nil)
	f.expectRepublish()

	if err := f.svc.RenameUser(context.Background(), "u1", "new"); err != nil {
		t.Fatalf("RenameUser: %v", err)
	}
}

// TestProvisioning_RenameUserRefusals covers what a rename will not do: act on
// an identity nothing declares, act on one the config file owns, or accept an
// empty name that would leave the user unreadable in a listing.
func TestProvisioning_RenameUserRefusals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		buckets []config.BucketConfig
		store   *provStore
		id      string
		newName string
		want    error
	}{
		{
			name:  "no id",
			store: &provStore{},
			want:  ErrUserRequired,
		},
		{
			name:  "no name",
			store: &provStore{users: []core.User{{ID: "u1", Name: "old"}}},
			id:    "u1",
			want:  ErrNameRequired,
		},
		{
			name:    "unknown user",
			store:   &provStore{},
			id:      "u1",
			newName: "new",
			want:    ErrUserNotFound,
		},
		{
			name: "a config-declared identity",
			buckets: []config.BucketConfig{
				{Name: "photos", Credentials: []config.CredentialConfig{{AccessKeyID: "AK", SecretAccessKey: "SK"}}},
			},
			store:   &provStore{},
			id:      "config:AK",
			newName: "new",
			want:    ErrConfigDeclared,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProvFixture(t, tc.buckets, tc.store)
			if err := f.svc.RenameUser(context.Background(), tc.id, tc.newName); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestProvisioning_SetGrantWritesWhateverIsThere verifies the upsert: the same
// call lands whether or not the grant already exists, which is what lets a
// caller declare access without first asking.
func TestProvisioning_SetGrantWritesWhateverIsThere(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		store *provStore
	}{
		{
			name:  "no grant yet",
			store: &provStore{users: []core.User{{ID: "u1", Name: "ci"}}, buckets: []core.Bucket{{Name: "photos"}}},
		},
		{
			name: "replacing what one carries",
			store: &provStore{
				users:   []core.User{{ID: "u1", Name: "ci"}},
				buckets: []core.Bucket{{Name: "photos"}},
				grants: []core.Grant{{
					UserID:      "u1",
					Resource:    core.BucketResource("photos"),
					Permissions: core.PermAll,
				}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProvFixture(t, nil, tc.store)
			var stored core.Grant
			f.store.EXPECT().SetGrant(gomock.Any(), gomock.Any()).
				DoAndReturn(func(_ context.Context, g *core.Grant) error {
					stored = *g
					return nil
				})
			f.expectRepublish()

			want := core.PermList | core.PermRead
			err := f.svc.SetGrant(context.Background(), "u1", core.BucketResource("photos"), want)
			if err != nil {
				t.Fatalf("SetGrant: %v", err)
			}
			if stored.Permissions != want {
				t.Errorf("stored permissions = %q, want %q", stored.Permissions, want)
			}
		})
	}
}

// TestProvisioning_SetGrantHoldsTheSameRulesAsCreate verifies the declarative
// path is not a way around the checks the imperative one makes. Both run the
// same validation, so a set that would write a meaningless grant is refused
// exactly as an add would be.
func TestProvisioning_SetGrantHoldsTheSameRulesAsCreate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		store    *provStore
		resource core.Resource
		perms    core.PermissionSet
		want     error
	}{
		{
			name:     "an identity nothing declares",
			store:    &provStore{},
			resource: core.BucketResource("photos"),
			perms:    core.PermRead,
			want:     ErrUserNotFound,
		},
		{
			name:     "a bucket nothing declares",
			store:    &provStore{users: []core.User{{ID: "u1", Name: "ci"}}},
			resource: core.BucketResource("nowhere"),
			perms:    core.PermRead,
			want:     ErrBucketNotFound,
		},
		{
			name:     "no permissions at all",
			store:    &provStore{users: []core.User{{ID: "u1", Name: "ci"}}, buckets: []core.Bucket{{Name: "photos"}}},
			resource: core.BucketResource("photos"),
			perms:    0,
			want:     ErrNoPermissions,
		},
		{
			name:     "an admin permission on a bucket",
			store:    &provStore{users: []core.User{{ID: "u1", Name: "ci"}}, buckets: []core.Bucket{{Name: "photos"}}},
			resource: core.BucketResource("photos"),
			perms:    core.PermAdminDrain,
			want:     ErrInvalidResource,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProvFixture(t, nil, tc.store)
			err := f.svc.SetGrant(context.Background(), "u1", tc.resource, tc.perms)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestProvisioning_UpdateSurfacesStoreFailures verifies a write the store
// refuses reaches the caller rather than being swallowed and reported as a
// success the registry never republished.
func TestProvisioning_UpdateSurfacesStoreFailures(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	for _, tc := range []struct {
		name  string
		setup func(f *provFixture)
		call  func(f *provFixture) error
	}{
		{
			name: "rename",
			setup: func(f *provFixture) {
				f.store.EXPECT().RenameUser(gomock.Any(), "u1", "new").Return(boom)
			},
			call: func(f *provFixture) error {
				return f.svc.RenameUser(context.Background(), "u1", "new")
			},
		},
		{
			name: "set grant",
			setup: func(f *provFixture) {
				f.store.EXPECT().SetGrant(gomock.Any(), gomock.Any()).Return(boom)
			},
			call: func(f *provFixture) error {
				return f.svc.SetGrant(context.Background(), "u1", core.BucketResource("photos"), core.PermRead)
			},
		},
		{
			name: "register a credential",
			setup: func(f *provFixture) {
				f.store.EXPECT().CreateCredential(gomock.Any(), gomock.Any()).Return(boom)
			},
			call: func(f *provFixture) error {
				_, err := f.svc.CreateCredential(context.Background(), "u1", "",
					Keypair{AccessKeyID: "AKIANEW", SecretAccessKey: "secret"})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProvFixture(t, nil, &provStore{
				users:   []core.User{{ID: "u1", Name: "ci"}},
				buckets: []core.Bucket{{Name: "photos"}},
			})
			tc.setup(f)
			// No republish is expected: a write that failed leaves the registry
			// describing what is still true.
			if err := tc.call(f); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want a wrap of boom", err)
			}
		})
	}
}

// TestProvisioning_UpdateSurfacesListingFailures verifies a store that cannot
// answer what exists stops the write rather than acting on a half-read view.
func TestProvisioning_UpdateSurfacesListingFailures(t *testing.T) {
	t.Parallel()

	boom := errors.New("listing unavailable")
	for _, tc := range []struct {
		name string
		call func(f *provFixture) error
	}{
		{
			name: "rename",
			call: func(f *provFixture) error {
				return f.svc.RenameUser(context.Background(), "u1", "new")
			},
		},
		{
			name: "set grant",
			call: func(f *provFixture) error {
				return f.svc.SetGrant(context.Background(), "u1", core.BucketResource("photos"), core.PermRead)
			},
		},
		{
			name: "register a credential",
			call: func(f *provFixture) error {
				_, err := f.svc.CreateCredential(context.Background(), "u1", "", Keypair{})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			store := opstest.NewMockProvisioningStore(ctrl)
			a := gomock.Any()
			store.EXPECT().ListBuckets(a).Return(nil, nil).AnyTimes()
			store.EXPECT().ListUsers(a).Return(nil, boom).AnyTimes()
			store.EXPECT().ListCredentials(a).Return(nil, nil).AnyTimes()
			store.EXPECT().ListGrants(a).Return(nil, nil).AnyTimes()

			f := &provFixture{store: store}
			f.svc = NewProvisioning(ProvisioningDeps{
				Store:  store,
				Config: NewConfigStore(&config.Config{}),
			})
			if err := tc.call(f); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want a wrap of boom", err)
			}
		})
	}
}
