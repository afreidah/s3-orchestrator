// -------------------------------------------------------------------------------
// Integration Tests - Provisioning Across Instances
//
// Author: Alex Freidah
//
// Two instances built through the production injector share one metadata store
// and one Redis. A credential issued through one must authenticate on the
// other without a restart or a reload.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/di"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// newSharedInstance builds one API instance through the production injector
// over the given SQLite file and Redis prefix, and starts its background
// services.
func newSharedInstance(t *testing.T, dbPath, redisPrefix string) do.Injector {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{
			ListenAddr:    "127.0.0.1:0",
			MaxObjectSize: 1 << 20,
		},
		Database: config.DatabaseConfig{Driver: "sqlite", Path: dbPath},
		Backends: []config.BackendConfig{{
			Name:            "b1",
			Endpoint:        "http://localhost:9999",
			Region:          "us-east-1",
			Bucket:          "bucket",
			AccessKeyID:     "AK",
			SecretAccessKey: "SK",
			ForcePathStyle:  true,
			QuotaBytes:      1 << 20,
		}},
		Buckets: []config.BucketConfig{{
			Name:        "photos",
			Credentials: []config.CredentialConfig{{AccessKeyID: "CONFIGKEY", SecretAccessKey: "CONFIGSECRET"}},
		}},
		Redis: &config.RedisConfig{Address: redisAddr(), KeyPrefix: redisPrefix},
	}
	if err := cfg.SetDefaultsAndValidate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	inj := di.NewInjector(di.InjectorDeps{
		Config: cfg, Mode: config.ModeAPI, LogLevel: new(slog.LevelVar), LogBuffer: telemetry.NewLogBuffer(),
	})

	mgr, err := do.Invoke[*lifecycle.Manager](inj)
	if err != nil {
		t.Fatalf("resolve lifecycle manager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go mgr.Run(ctx)
	t.Cleanup(func() {
		cancel()
		mgr.Stop(5 * time.Second)
		_ = inj.Shutdown()
	})
	return inj
}

// TestProvisioning_CredentialReachesEveryInstance issues a credential through
// one instance and requires the other to authenticate it. Without a way to
// hear about the change, the second instance keeps the registry it built at
// startup and rejects the credential until it restarts.
func TestProvisioning_CredentialReachesEveryInstance(t *testing.T) {
	newRedisClient(t) // skips when Redis is not reachable
	dbPath := filepath.Join(t.TempDir(), "shared.db")
	prefix := fmt.Sprintf("prov_%d", time.Now().UnixNano())
	writer := newSharedInstance(t, dbPath, prefix)
	reader := newSharedInstance(t, dbPath, prefix)

	svc, err := do.Invoke[*ops.Services](writer)
	if err != nil {
		t.Fatalf("resolve ops: %v", err)
	}
	srv, err := do.Invoke[*s3api.Server](reader)
	if err != nil {
		t.Fatalf("resolve reader server: %v", err)
	}

	ctx := context.Background()
	user, err := svc.Provision.CreateUser(ctx, "propagated")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cred, err := svc.Provision.CreateCredential(ctx, user.ID, "test", ops.Keypair{})
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := srv.GetBucketAuth().AuthenticateSecret(cred.AccessKeyID, cred.Secret); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the other instance still rejects a credential issued 5s ago")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
