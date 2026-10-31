// -------------------------------------------------------------------------------
// DI - Bucket Registry Assembly Tests
//
// Author: Alex Freidah
//
// Covers the wiring: resolving the provisioning store, failing when it is absent
// or unreadable, and handing the merged view to the registry. The merge rules
// themselves belong to the provisioning package and are tested there.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samber/do/v2"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/cors"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
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

// TestLoadProvisioning_MergesBothSources verifies loading builds a registry a
// stored keypair authenticates against, which is what a boot and a reload both
// call, and publishes nothing while doing it.
func TestLoadProvisioning_MergesBothSources(t *testing.T) {
	t.Parallel()

	store := storetest.NewMockProvisioningStore(gomock.NewController(t))
	a := gomock.Any()
	store.EXPECT().ListBuckets(a).
		Return([]core.Bucket{{Name: "stored", MaxMultipartUploads: 7}}, nil).AnyTimes()
	store.EXPECT().ListUsers(a).Return([]core.User{{ID: "u1", Name: "ci"}}, nil).AnyTimes()
	store.EXPECT().ListCredentials(a).
		Return([]core.Credential{{AccessKeyID: "AK", UserID: "u1", Secret: "SK"}}, nil).AnyTimes()
	store.EXPECT().ListGrants(a).
		Return([]core.Grant{{UserID: "u1", Resource: core.BucketResource("stored")}}, nil).AnyTimes()

	inj := do.New()
	do.ProvideValue[core.ProvisioningStore](inj, store)
	declared := provisioning.NewDeclared()
	do.ProvideValue(inj, declared)

	p, err := LoadProvisioning(context.Background(), inj,
		&config.Config{Buckets: []config.BucketConfig{{Name: "from-config"}}})
	if err != nil {
		t.Fatalf("LoadProvisioning: %v", err)
	}
	if got := p.registry.MaxMultipartUploads("stored"); got != 7 {
		t.Errorf("stored bucket limit = %d, want 7", got)
	}
	if len(declared.Names()) != 0 {
		t.Errorf("loading published %v", declared.Names())
	}
}

// TestLoadProvisioning_ReportsNotices verifies what the merge found reaches
// the caller that logs it, rather than being discarded.
func TestLoadProvisioning_ReportsNotices(t *testing.T) {
	t.Parallel()

	store := storetest.NewMockProvisioningStore(gomock.NewController(t))
	a := gomock.Any()
	store.EXPECT().ListBuckets(a).Return([]core.Bucket{{Name: "photos"}}, nil).AnyTimes()
	store.EXPECT().ListUsers(a).Return(nil, nil).AnyTimes()
	store.EXPECT().ListCredentials(a).Return(nil, nil).AnyTimes()
	store.EXPECT().ListGrants(a).
		Return([]core.Grant{{UserID: "ghost", Resource: core.BucketResource("nowhere")}}, nil).AnyTimes()

	inj := do.New()
	do.ProvideValue[core.ProvisioningStore](inj, store)

	// photos collides with the config bucket and the grant names nothing, so
	// loading serves through both and reports each.
	p, err := LoadProvisioning(context.Background(), inj,
		&config.Config{Buckets: []config.BucketConfig{{Name: "photos"}}})
	if err != nil {
		t.Fatalf("LoadProvisioning: %v", err)
	}
	if len(p.registry.Notices()) != 2 {
		t.Fatalf("notices = %d, want 2", len(p.registry.Notices()))
	}
}

// TestLoadProvisioning_StoreUnavailable verifies a store that cannot be read
// fails rather than falling back to config alone, which would answer 403 to
// callers holding stored credentials.
func TestLoadProvisioning_StoreUnavailable(t *testing.T) {
	t.Parallel()

	if _, err := LoadProvisioning(context.Background(), do.New(), &config.Config{}); err == nil {
		t.Fatal("loading succeeded with no store registered")
	}
}

// TestLoadProvisioning_ReadFailurePropagates verifies a store that answers an
// error fails loading rather than serving a partial picture.
func TestLoadProvisioning_ReadFailurePropagates(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	store := storetest.NewMockProvisioningStore(gomock.NewController(t))
	store.EXPECT().ListBuckets(gomock.Any()).Return(nil, boom)

	inj := do.New()
	do.ProvideValue[core.ProvisioningStore](inj, store)

	if _, err := LoadProvisioning(context.Background(), inj, &config.Config{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want a wrap of boom", err)
	}
}

// liveConfig registers the operations layer holding cfg as the configuration in
// force, which is what a provisioning change rebuilds from.
func liveConfig(inj do.Injector, cfg *config.Config) {
	store := &ops.ConfigStore{}
	store.Store(cfg)
	do.ProvideValue(inj, &ops.Services{Config: store})
}

// -------------------------------------------------------------------------
// REGISTRY PUBLISHER
// -------------------------------------------------------------------------

// TestRegistryPublisher_SwapsTheRunningRegistry verifies a provisioning change
// takes effect on the next request: the publisher reassembles from the store
// and installs both the credential registry and the bucket CORS rules on the
// serving instance.
func TestRegistryPublisher_SwapsTheRunningRegistry(t *testing.T) {
	t.Parallel()

	store := storetest.NewMockProvisioningStore(gomock.NewController(t))
	a := gomock.Any()
	store.EXPECT().ListBuckets(a).Return(nil, nil).AnyTimes()
	store.EXPECT().ListUsers(a).Return([]core.User{{ID: "u1", Name: "ci"}}, nil).AnyTimes()
	store.EXPECT().ListCredentials(a).
		Return([]core.Credential{{AccessKeyID: "AK", UserID: "u1", Secret: "SK"}}, nil).AnyTimes()
	store.EXPECT().ListGrants(a).
		Return([]core.Grant{{UserID: "u1", Resource: core.BucketResource("photos")}}, nil).AnyTimes()

	inj := do.New()
	do.ProvideValue[core.ProvisioningStore](inj, store)
	do.ProvideValue(inj, provisioning.NewDeclared())
	do.ProvideValue(inj, config.ModeAPI)
	liveConfig(inj, &config.Config{Buckets: []config.BucketConfig{{
		Name: "photos",
		CORS: []config.CORSRule{{AllowedOrigins: []string{"https://app.example.com"}, AllowedMethods: []string{"GET"}}},
	}}})

	srv := &s3api.Server{}
	do.ProvideValue(inj, srv)
	policy := cors.New(s3api.BucketFromPath, s3api.WriteS3Error)
	do.ProvideValue(inj, policy)

	before := srv.GetBucketAuth()
	if err := NewRegistryPublisher(inj).Republish(context.Background()); err != nil {
		t.Fatalf("Republish: %v", err)
	}
	after := srv.GetBucketAuth()
	if after == nil || after == before {
		t.Fatal("the server is still serving the registry it had before")
	}

	preflight := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/photos/key", http.NoBody)
	preflight.Header.Set("Origin", "https://app.example.com")
	preflight.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	policy.Middleware(http.NotFoundHandler()).ServeHTTP(w, preflight)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("preflight Allow-Origin = %q after Republish; the bucket's CORS rules were not installed", got)
	}
}

// TestRegistryPublisher_WorkerModeRefreshesDeclaredOnly rebuilds the declared
// bucket set in a mode that serves no S3 API, without resolving a server or a
// CORS policy that mode never builds. It rebuilds from the configuration in
// force, not the one the process started with, which a reload has replaced.
func TestRegistryPublisher_WorkerModeRefreshesDeclaredOnly(t *testing.T) {
	t.Parallel()

	inj := do.New()
	do.ProvideValue[core.ProvisioningStore](inj, emptyProvisioningStore(t))
	declared := provisioning.NewDeclared()
	do.ProvideValue(inj, declared)
	do.ProvideValue(inj, config.ModeWorker)
	do.ProvideValue(inj, &config.Config{Buckets: []config.BucketConfig{{Name: "startup"}}})
	liveConfig(inj, &config.Config{Buckets: []config.BucketConfig{{Name: "photos"}}})

	if err := NewRegistryPublisher(inj).Republish(context.Background()); err != nil {
		t.Fatalf("Republish: %v", err)
	}
	if got := declared.Buckets(); len(got) != 1 || got[0].Name != "photos" {
		t.Errorf("declared buckets = %+v, want photos", got)
	}
}

// TestRegistryPublisher_MissingDependencyFails verifies a publisher that cannot
// reach what it needs reports it, rather than reporting a change as live when
// nothing was swapped.
func TestRegistryPublisher_MissingDependencyFails(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(do.Injector)
	}{
		{"no config", func(do.Injector) {}},
		{"no store", func(i do.Injector) {
			liveConfig(i, &config.Config{})
			do.ProvideValue(i, config.ModeAPI)
		}},
		{"no mode", func(i do.Injector) {
			liveConfig(i, &config.Config{})
			do.ProvideValue[core.ProvisioningStore](i, emptyProvisioningStore(t))
			do.ProvideValue(i, provisioning.NewDeclared())
		}},
		{"no server", func(i do.Injector) {
			liveConfig(i, &config.Config{})
			do.ProvideValue(i, config.ModeAPI)
			do.ProvideValue[core.ProvisioningStore](i, emptyProvisioningStore(t))
			do.ProvideValue(i, provisioning.NewDeclared())
		}},
		{"no CORS policy", func(i do.Injector) {
			liveConfig(i, &config.Config{})
			do.ProvideValue(i, config.ModeAPI)
			do.ProvideValue[core.ProvisioningStore](i, emptyProvisioningStore(t))
			do.ProvideValue(i, provisioning.NewDeclared())
			do.ProvideValue(i, &s3api.Server{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inj := do.New()
			tc.setup(inj)
			if err := NewRegistryPublisher(inj).Republish(context.Background()); err == nil {
				t.Fatal("Republish succeeded with a dependency missing")
			}
		})
	}
}

// TestLoadProvisioning_RejectsAmbiguousConfig verifies a credential two
// config buckets both claim still fails loading, so the store merge did not
// weaken the backstop that keeps it from becoming a cross-bucket grant.
func TestLoadProvisioning_RejectsAmbiguousConfig(t *testing.T) {
	t.Parallel()

	inj := do.New()
	do.ProvideValue[core.ProvisioningStore](inj, emptyProvisioningStore(t))

	_, err := LoadProvisioning(context.Background(), inj, &config.Config{
		Buckets: []config.BucketConfig{
			{Name: "a", Credentials: []config.CredentialConfig{
				{AccessKeyID: "SAME", SecretAccessKey: "a-secret"},
			}},
			{Name: "b", Credentials: []config.CredentialConfig{
				{AccessKeyID: "SAME", SecretAccessKey: "b-secret"},
			}},
		},
	})
	if err == nil {
		t.Fatal("assembly accepted an access key claimed by two buckets")
	}
}
