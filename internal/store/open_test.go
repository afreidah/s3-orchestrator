// -------------------------------------------------------------------------------
// Store Opening - Tests
//
// Author: Alex Freidah
//
// Covers the driver dispatch and the boot order Prepare holds both the server
// and the sync command to.
// -------------------------------------------------------------------------------

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// testInstanceID is the instance id the opened stores claim cleanup rows under.
const testInstanceID = "store-test"

// TestOpen_InvalidDriver covers the default branch of the driver dispatch.
func TestOpen_InvalidDriver(t *testing.T) {
	t.Parallel()
	if _, err := Open(context.Background(), &config.DatabaseConfig{Driver: "bogus"}, nil, testInstanceID); err == nil {
		t.Fatal("expected error for unsupported driver, got nil")
	}
}

// TestOpen_SQLiteInMemory covers the sqlite branch with an in-memory database.
func TestOpen_SQLiteInMemory(t *testing.T) {
	t.Parallel()
	s, err := Open(context.Background(), &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"}, nil, testInstanceID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Close()
}

// TestOpen_PostgresInvalidConfig covers the postgres branch. A unit test cannot
// open a real Postgres, so it uses a config that fails fast.
func TestOpen_PostgresInvalidConfig(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := Open(ctx, &config.DatabaseConfig{
		Driver:   "postgres",
		Host:     "127.0.0.1",
		Port:     1,
		Database: "nope",
		User:     "nope",
		Password: "nope",
		SSLMode:  "disable",
	}, nil, testInstanceID)
	if err == nil {
		t.Fatal("expected error connecting to unreachable postgres, got nil")
	}
}

// TestPrepare_RunsTheBootStepsInOrder pins migrations before the schema check
// before the quota sync, since each step needs the one before it.
func TestPrepare_RunsTheBootStepsInOrder(t *testing.T) {
	t.Parallel()
	admin := storetest.NewMockLifecycleAdmin(gomock.NewController(t))
	ctx := context.Background()
	gomock.InOrder(
		admin.EXPECT().RunMigrations(ctx).Return(nil),
		admin.EXPECT().VerifySchemaVersion(ctx).Return(nil),
		admin.EXPECT().SyncQuotaLimits(ctx, nil).Return(nil),
	)
	if err := Prepare(ctx, admin, nil); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
}

// TestPrepare_StopsAtTheFirstFailure verifies a schema this build does not
// expect fails the boot before anything is written against it.
func TestPrepare_StopsAtTheFirstFailure(t *testing.T) {
	t.Parallel()
	admin := storetest.NewMockLifecycleAdmin(gomock.NewController(t))
	ctx := context.Background()
	refused := errors.New("schema is newer than this build")
	admin.EXPECT().RunMigrations(ctx).Return(nil)
	admin.EXPECT().VerifySchemaVersion(ctx).Return(refused)
	if err := Prepare(ctx, admin, nil); !errors.Is(err, refused) {
		t.Errorf("err = %v, want the schema refusal", err)
	}
}
