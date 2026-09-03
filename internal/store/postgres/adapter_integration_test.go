// -------------------------------------------------------------------------------
// Postgres TxAdapter - Per-Method Integration Tests
//
// Author: Alex Freidah
//
// Direct coverage for every method on pgTxAdapter against a real PostgreSQL
// instance via testcontainers. Tagged "integration" so the default unit
// test run skips it; the integration-test target in the Makefile and the
// integration job in CI run it.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// -------------------------------------------------------------------------
// SHARED CONTAINER FIXTURE
// -------------------------------------------------------------------------

// The lazily started Postgres testcontainer shared by the whole suite.
var (
	pgFixtureOnce sync.Once
	pgFixture     *Store
	pgFixtureErr  error
)

// adapterPgStore lazily starts a Postgres testcontainer once for the
// suite, applies the embedded migrations, seeds quota rows for the
// test backends, and returns a *Store whose pool every test can issue
// transactions against. Container lifecycle relies on Ryuk for
// teardown.
func adapterPgStore(t *testing.T) *Store {
	t.Helper()
	pgFixtureOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// Debian rather than alpine: musl has no locale data, so every
		// collation degrades to byte order and a query that forgot its
		// COLLATE "C" would pass every ordering assertion here. glibc
		// orders text the way a real deployment does.
		ctr, err := tcpostgres.Run(ctx,
			"postgres:16",
			tcpostgres.WithDatabase("adapter_test"),
			tcpostgres.WithUsername("s3proxy"),
			tcpostgres.WithPassword("s3proxy"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(30*time.Second)),
		)
		if err != nil {
			pgFixtureErr = fmt.Errorf("start postgres: %w", err)
			return
		}
		host, err := ctr.Host(ctx)
		if err != nil {
			pgFixtureErr = fmt.Errorf("postgres host: %w", err)
			return
		}
		port, err := ctr.MappedPort(ctx, "5432/tcp")
		if err != nil {
			pgFixtureErr = fmt.Errorf("postgres port: %w", err)
			return
		}

		s, err := NewStore(ctx, &config.DatabaseConfig{
			Host:     host,
			Port:     int(port.Num()),
			Database: "adapter_test",
			User:     "s3proxy",
			Password: "s3proxy",
			SSLMode:  "disable",
			MaxConns: 4,
			MinConns: 1,
		}, nil)
		if err != nil {
			pgFixtureErr = fmt.Errorf("NewStore: %w", err)
			return
		}
		if err := s.RunMigrations(ctx); err != nil {
			pgFixtureErr = fmt.Errorf("RunMigrations: %w", err)
			return
		}
		if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{
			{Name: "backend-a", QuotaBytes: 1 << 30},
			{Name: "backend-b", QuotaBytes: 1 << 30},
		}); err != nil {
			pgFixtureErr = fmt.Errorf("SyncQuotaLimits: %w", err)
			return
		}
		pgFixture = s
	})
	if pgFixtureErr != nil {
		t.Fatalf("postgres fixture: %v", pgFixtureErr)
	}
	if pgFixture == nil {
		t.Fatal("postgres fixture nil")
	}
	return pgFixture
}

// withPgAdapter opens a tx on the shared *Store, hands the wrapping
// adapter to fn, and rolls back on return so each test starts with a
// clean slate. Tests that need rows committed call s.* methods on the
// outer *Store before opening the adapter tx.
func withPgAdapter(t *testing.T, s *Store, fn func(*pgTxAdapter)) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	fn(&pgTxAdapter{q: s.queries.WithTx(tx)})
}

// uniqueKey returns a key prefixed with the test name so suites can
// run in parallel against the shared container without colliding on
// rows committed outside the rollback-scoped adapter tx.
func uniqueKey(t *testing.T, suffix string) string {
	t.Helper()
	return t.Name() + "/" + suffix
}

// -------------------------------------------------------------------------
// AcquireKeyLock
// -------------------------------------------------------------------------

// TestPgAdapter_AcquireKeyLock_Succeeds verifies the advisory key
// lock returns a nil error against a real Postgres tx.
func TestPgAdapter_AcquireKeyLock_Succeeds(t *testing.T) {
	s := adapterPgStore(t)
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		if err := a.AcquireKeyLock(context.Background(), uniqueKey(t, "k")); err != nil {
			t.Errorf("AcquireKeyLock: %v", err)
		}
	})
}

// -------------------------------------------------------------------------
// PENDING TX
// -------------------------------------------------------------------------

// TestPgAdapter_ClaimPending_TrueWhenInserted verifies the FOR UPDATE
// lock returns true for an existing pending row.
func TestPgAdapter_ClaimPending_TrueWhenInserted(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	intentID := uniqueKey(t, "intent")
	if err := s.InsertPending(ctx, &core.PendingObject{
		IntentID: intentID, ObjectKey: uniqueKey(t, "k"), BackendName: "backend-a", SizeBytes: 1,
	}); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	defer func() { _ = s.DeletePending(ctx, intentID) }()

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		got, err := a.ClaimPending(ctx, intentID)
		if err != nil {
			t.Fatalf("ClaimPending: %v", err)
		}
		if !got {
			t.Error("expected true for existing intent")
		}
	})
}

// TestPgAdapter_ClaimPending_FalseWhenMissing verifies the lock
// translates pgx.ErrNoRows into (false, nil).
func TestPgAdapter_ClaimPending_FalseWhenMissing(t *testing.T) {
	s := adapterPgStore(t)
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		got, err := a.ClaimPending(context.Background(), uniqueKey(t, "missing"))
		if err != nil {
			t.Fatalf("ClaimPending: %v", err)
		}
		if got {
			t.Error("expected false for missing intent")
		}
	})
}

// TestPgAdapter_InsertPending_PreservesAllFields verifies a fully
// populated pending row inserted through the adapter round-trips
// every field through GetStalePending. The adapter call is committed
// (not rolled back) so the read-back can see the row.
func TestPgAdapter_InsertPending_PreservesAllFields(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	intentID := uniqueKey(t, "intent")
	want := core.PendingObject{
		IntentID:      intentID,
		ObjectKey:     uniqueKey(t, "k"),
		BackendName:   "backend-a",
		SizeBytes:     200,
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         "kid-1",
		PlaintextSize: 180,
		ContentHash:   "abc123",
	}
	if err := s.InsertPending(ctx, &want); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	defer func() { _ = s.DeletePending(ctx, intentID) }()

	rows, err := s.GetStalePending(ctx, time.Now().Add(time.Hour), 1000)
	if err != nil {
		t.Fatalf("GetStalePending: %v", err)
	}
	var got *core.PendingObject
	for i := range rows {
		if rows[i].IntentID == intentID {
			got = &rows[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("intent %s not found in stale pending list", intentID)
	}
	if got.ObjectKey != want.ObjectKey || got.BackendName != want.BackendName || got.SizeBytes != want.SizeBytes {
		t.Errorf("required fields mismatch: got %+v want %+v", got, want)
	}
	if !got.Encrypted || got.KeyID != "kid-1" || got.PlaintextSize != 180 || got.ContentHash != "abc123" {
		t.Errorf("optional fields mismatch: %+v", got)
	}
	if string(got.EncryptionKey) != "packed" {
		t.Errorf("EncryptionKey not preserved: %v", got.EncryptionKey)
	}
}

// TestPgInsertPending_NullableFieldsOmitted verifies that empty optional
// fields land as SQL NULL and read back through GetStalePending as zero
// values, rather than as a stored zero an encrypted-object check would
// mistake for real metadata.
func TestPgInsertPending_NullableFieldsOmitted(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	intentID := uniqueKey(t, "intent")
	if err := s.InsertPending(ctx, &core.PendingObject{
		IntentID: intentID, ObjectKey: uniqueKey(t, "k"), BackendName: "backend-a", SizeBytes: 5,
	}); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	defer func() { _ = s.DeletePending(ctx, intentID) }()

	rows, err := s.GetStalePending(ctx, time.Now().Add(time.Hour), 1000)
	if err != nil {
		t.Fatalf("GetStalePending: %v", err)
	}
	for i := range rows {
		if rows[i].IntentID != intentID {
			continue
		}
		got := rows[i]
		if got.KeyID != "" || got.PlaintextSize != 0 || got.ContentHash != "" {
			t.Errorf("expected zero optional fields (SQL NULL), got %+v", got)
		}
		return
	}
	t.Fatalf("intent %s not found", intentID)
}

// TestPgAdapter_DeletePending_RemovesRow verifies the delete clears
// the row.
func TestPgAdapter_DeletePending_RemovesRow(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	intentID := uniqueKey(t, "intent")
	if err := s.InsertPending(ctx, &core.PendingObject{
		IntentID: intentID, ObjectKey: uniqueKey(t, "k"), BackendName: "backend-a", SizeBytes: 1,
	}); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		if err := a.DeletePending(ctx, intentID); err != nil {
			t.Fatalf("DeletePending: %v", err)
		}
		got, err := a.ClaimPending(ctx, intentID)
		if err != nil {
			t.Fatalf("ClaimPending: %v", err)
		}
		if got {
			t.Error("intent still present after DeletePending")
		}
	})
}

// -------------------------------------------------------------------------
// OBJECTS TX
// -------------------------------------------------------------------------

// TestPgAdapter_GetExistingCopiesForUpdate_ReturnsAllCopies verifies
// the FOR UPDATE read returns every copy of a key with the correct
// backend, size, and timestamp fields.
func TestPgAdapter_GetExistingCopiesForUpdate_ReturnsAllCopies(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()
	if _, _, err := s.RecordReplica(ctx, key, "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		copies, err := a.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 2 {
			t.Fatalf("expected 2 copies, got %d", len(copies))
		}
		seen := map[string]int64{}
		for _, ec := range copies {
			seen[ec.BackendName] = ec.SizeBytes
			if ec.CreatedAt.IsZero() {
				t.Errorf("CreatedAt zero for %s", ec.BackendName)
			}
		}
		if seen["backend-a"] != 100 || seen["backend-b"] != 100 {
			t.Errorf("expected both copies at 100 bytes, got %v", seen)
		}
	})
}

// TestPgAdapter_GetExistingCopiesForUpdate_EmptyForUnknownKey verifies
// the read returns an empty slice (no error) when the key has no
// rows.
func TestPgAdapter_GetExistingCopiesForUpdate_EmptyForUnknownKey(t *testing.T) {
	s := adapterPgStore(t)
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		copies, err := a.GetExistingCopiesForUpdate(context.Background(), uniqueKey(t, "missing"))
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 0 {
			t.Errorf("expected empty slice for missing key, got %+v", copies)
		}
	})
}

// TestPgAdapter_InsertObjectLocation_PreservesEncryptionFields
// verifies the insert lands every encryption + integrity field through
// GetAllObjectLocations.
func TestPgAdapter_InsertObjectLocation_PreservesEncryptionFields(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	form := &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         "kid-1",
		PlaintextSize: 50,
		ContentHash:   "abc123",
	}
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 75, Form: form}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()

	locs, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 location, got %d", len(locs))
	}
	got := locs[0]
	if got.SizeBytes != 75 || !got.Encrypted || got.KeyID != "kid-1" || got.PlaintextSize != 50 || got.ContentHash != "abc123" {
		t.Errorf("encryption fields not preserved: %+v", got)
	}
	if string(got.EncryptionKey) != "packed" {
		t.Errorf("EncryptionKey not preserved: %v", got.EncryptionKey)
	}
}

// TestPgAdapter_DeleteObjectCopies_RemovesAllRows verifies every copy
// of the key is removed while copies of other keys remain.
func TestPgAdapter_DeleteObjectCopies_RemovesAllRows(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	other := uniqueKey(t, "other")
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("RecordObject(target): %v", err)
	}
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: other, Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("RecordObject(other): %v", err)
	}
	defer func() {
		_, _ = s.DeleteObject(ctx, key)
		_, _ = s.DeleteObject(ctx, other)
	}()

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		if err := a.DeleteObjectCopies(ctx, key); err != nil {
			t.Fatalf("DeleteObjectCopies: %v", err)
		}
		copies, err := a.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate(target): %v", err)
		}
		if len(copies) != 0 {
			t.Errorf("target copies still present: %+v", copies)
		}
		copies, err = a.GetExistingCopiesForUpdate(ctx, other)
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate(other): %v", err)
		}
		if len(copies) != 1 {
			t.Errorf("other key untouched expected, got %+v", copies)
		}
	})
}

// TestPgAdapter_CheckObjectExistsOnBackend verifies both the present
// and missing branches of the existence probe.
func TestPgAdapter_CheckObjectExistsOnBackend(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		got, err := a.CheckObjectExistsOnBackend(ctx, key, "backend-a")
		if err != nil {
			t.Fatalf("CheckObjectExistsOnBackend(present): %v", err)
		}
		if !got {
			t.Error("expected true for present (key, backend)")
		}
		got, err = a.CheckObjectExistsOnBackend(ctx, key, "backend-b")
		if err != nil {
			t.Fatalf("CheckObjectExistsOnBackend(missing): %v", err)
		}
		if got {
			t.Error("expected false for missing (key, backend)")
		}
	})
}

// TestPgAdapter_LockObjectOnBackend_ReturnsRow verifies the lock
// returns the row payload when the (key, backend) row exists,
// preserving every encryption and integrity column.
func TestPgAdapter_LockObjectOnBackend_ReturnsRow(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	form := &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: []byte("packed"),
		KeyID:         "kid-1",
		PlaintextSize: 50,
		ContentHash:   "hash",
	}
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 75, Form: form}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		loc, ok, err := a.LockObjectOnBackend(ctx, key, "backend-a")
		if err != nil {
			t.Fatalf("LockObjectOnBackend: %v", err)
		}
		if !ok {
			t.Fatal("expected row present")
		}
		if loc.SizeBytes != 75 || !loc.Encrypted || loc.KeyID != "kid-1" || loc.PlaintextSize != 50 || loc.ContentHash != "hash" {
			t.Errorf("payload mismatch: %+v", loc)
		}
		if string(loc.EncryptionKey) != "packed" {
			t.Errorf("EncryptionKey not preserved: %v", loc.EncryptionKey)
		}
	})
}

// TestPgAdapter_LockObjectOnBackend_FalseWhenMissing verifies the
// lock translates pgx.ErrNoRows into (nil, false, nil).
func TestPgAdapter_LockObjectOnBackend_FalseWhenMissing(t *testing.T) {
	s := adapterPgStore(t)
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		loc, ok, err := a.LockObjectOnBackend(context.Background(), uniqueKey(t, "missing"), "backend-a")
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				t.Fatal("ErrNoRows leaked to caller; should be (nil, false, nil)")
			}
			t.Fatalf("LockObjectOnBackend: %v", err)
		}
		if ok || loc != nil {
			t.Errorf("expected (nil, false, nil) for missing row, got loc=%+v ok=%v", loc, ok)
		}
	})
}

// TestPgAdapter_DeleteObjectFromBackend_RemovesOneRow verifies only
// the targeted (key, backend) row is removed; replicas remain.
func TestPgAdapter_DeleteObjectFromBackend_RemovesOneRow(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	if _, _, err := s.RecordReplica(ctx, key, "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		if err := a.DeleteObjectFromBackend(ctx, key, "backend-a"); err != nil {
			t.Fatalf("DeleteObjectFromBackend: %v", err)
		}
		copies, err := a.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 1 || copies[0].BackendName != "backend-b" {
			t.Errorf("expected only backend-b after delete, got %+v", copies)
		}
	})
}

// TestPgAdapter_InsertObjectLocationIfNotExists_BothBranches verifies
// the conditional insert returns true when a row is newly created
// and false when one already exists for (key, backend).
func TestPgAdapter_InsertObjectLocationIfNotExists_BothBranches(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	defer func() { _, _ = s.DeleteObject(ctx, key) }()

	if got, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: "backend-a", Size: 50}); err != nil || got != core.ImportInserted {
		t.Fatalf("ImportObject(insert): got=%s err=%v, want inserted", got, err)
	}
	if got, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: "backend-a", Size: 50}); err != nil || got != core.ImportSkippedExisting {
		t.Errorf("ImportObject(idempotent): got=%s err=%v, want skipped_existing", got, err)
	}
}

// TestPgAdapter_InsertReplicaConditional_InsertsWhenSourceExists
// verifies the conditional insert succeeds when the source row is
// present and the target row is absent.
func TestPgAdapter_InsertReplicaConditional_InsertsWhenSourceExists(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()

	if _, ok, err := s.RecordReplica(ctx, key, "backend-b", "backend-a"); err != nil || !ok {
		t.Fatalf("RecordReplica(insert): got ok=%v err=%v, want (true, nil)", ok, err)
	}

	locs, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if len(locs) != 2 {
		t.Errorf("expected 2 copies after replica, got %d", len(locs))
	}
}

// TestPgAdapter_InsertReplicaConditional_SkipsWhenSourceMissing
// verifies the conditional insert returns false (without error) when
// the source row has been removed.
func TestPgAdapter_InsertReplicaConditional_SkipsWhenSourceMissing(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		_, got, err := a.InsertReplicaConditional(ctx, uniqueKey(t, "missing"), "backend-b", "backend-a")
		if err != nil {
			t.Fatalf("InsertReplicaConditional: %v", err)
		}
		if got {
			t.Error("expected false when source is missing")
		}
	})
}

// -------------------------------------------------------------------------
// CLEANUP TX
// -------------------------------------------------------------------------

// TestPgAdapter_SumAndDeleteCleanupQueueRows_DeletesAndReturnsTotals
// verifies the sum-then-delete returns the row count and total bytes
// of the rows it removed.
func TestPgAdapter_SumAndDeleteCleanupQueueRows_DeletesAndReturnsTotals(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	if err := s.EnqueueCleanup(ctx, "backend-a", key, "test", 256); err != nil {
		t.Fatalf("EnqueueCleanup(1): %v", err)
	}
	if err := s.EnqueueCleanup(ctx, "backend-a", key, "test", 256); err != nil {
		t.Fatalf("EnqueueCleanup(2): %v", err)
	}

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		count, total, err := a.SumAndDeleteCleanupQueueRows(ctx, key, "backend-a")
		if err != nil {
			t.Fatalf("SumAndDeleteCleanupQueueRows: %v", err)
		}
		if count != 2 || total != 512 {
			t.Errorf("count=%d total=%d, want 2 and 512", count, total)
		}
	})
}

// TestPgAdapter_SumAndDeleteCleanupQueueRows_NoRowsReturnsZero
// verifies the sum-then-delete returns (0, 0, nil) when no matching
// rows exist.
func TestPgAdapter_SumAndDeleteCleanupQueueRows_NoRowsReturnsZero(t *testing.T) {
	s := adapterPgStore(t)
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		count, total, err := a.SumAndDeleteCleanupQueueRows(context.Background(), uniqueKey(t, "missing"), "backend-a")
		if err != nil {
			t.Fatalf("SumAndDeleteCleanupQueueRows: %v", err)
		}
		if count != 0 || total != 0 {
			t.Errorf("count=%d total=%d, want 0 and 0", count, total)
		}
	})
}

// -------------------------------------------------------------------------
// QUOTA TX
// -------------------------------------------------------------------------

// TestPgAdapter_IncrementBackendQuota_AddsBytesUsed verifies the
// increment updates bytes_used and returns nil error inside a tx.
func TestPgAdapter_IncrementBackendQuota_AddsBytesUsed(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		if err := a.IncrementBackendQuota(ctx, "backend-a", 500); err != nil {
			t.Fatalf("IncrementBackendQuota: %v", err)
		}
	})
}

// TestPgAdapter_IncrementBackendQuota_ReturnsErrNoSpaceWhenExceeded
// verifies the guarded UPDATE returns ErrNoSpaceAvailable when the
// quota ceiling would be exceeded.
func TestPgAdapter_IncrementBackendQuota_ReturnsErrNoSpaceWhenExceeded(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx,
		`UPDATE backend_quotas SET bytes_limit = 100 WHERE backend_name = $1`, "backend-a",
	); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() {
		_, _ = s.pool.Exec(ctx,
			`UPDATE backend_quotas SET bytes_limit = $1 WHERE backend_name = $2`, int64(1<<30), "backend-a",
		)
	}()

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		err := a.IncrementBackendQuota(ctx, "backend-a", 200)
		if !errors.Is(err, core.ErrNoSpaceAvailable) {
			t.Errorf("got %v, want ErrNoSpaceAvailable", err)
		}
	})
}

// TestPgAdapter_DecrementBackendQuota_NoErrorOnZero verifies the
// decrement returns nil when there's no row drift.
func TestPgAdapter_DecrementBackendQuota_NoErrorOnZero(t *testing.T) {
	s := adapterPgStore(t)
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		if err := a.DecrementBackendQuota(context.Background(), "backend-b", 100); err != nil {
			t.Errorf("DecrementBackendQuota: %v", err)
		}
	})
}

// TestPgAdapter_DecrementOrphanBytes_NoErrorOnZero verifies the
// orphan_bytes decrement returns nil even when the counter is at
// zero (the SQL clamps via MAX(0, ...)).
func TestPgAdapter_DecrementOrphanBytes_NoErrorOnZero(t *testing.T) {
	s := adapterPgStore(t)
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		if err := a.DecrementOrphanBytes(context.Background(), "backend-a", 9999); err != nil {
			t.Errorf("DecrementOrphanBytes: %v", err)
		}
	})
}

// TestPgAdapter_GetExistingCopiesForUpdate_CarriesEncryptionState verifies the
// Postgres locked re-read reports the same encryption state the SQLite adapter
// does. RemoveExcessCopy's guard reads these two fields, so the engines must
// agree or the guard protects objects on one backend and not the other.
func TestPgAdapter_GetExistingCopiesForUpdate_CarriesEncryptionState(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "enc")

	form := &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: []byte("wrapped-dek"),
		KeyID:         "key-1",
		PlaintextSize: 1024,
	}
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 1100, Form: form}); err != nil {
		t.Fatalf("RecordObject encrypted: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()
	if _, _, err := s.RecordReplica(ctx, key, "backend-b", "backend-a"); err != nil {
		t.Fatalf("RecordReplica: %v", err)
	}

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		copies, err := a.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 2 {
			t.Fatalf("expected 2 copies, got %d", len(copies))
		}
		for _, ec := range copies {
			if !ec.Encrypted {
				t.Errorf("%s: Encrypted = false, want true", ec.BackendName)
			}
			if !ec.HasDEK {
				t.Errorf("%s: HasDEK = false, want true (replication copies the key)", ec.BackendName)
			}
		}
	})
}

// TestPgAdapter_GetExistingCopiesForUpdate_ReportsUnencryptedCopy verifies a
// plain object reports neither flag, so the guard stays inert for copy sets
// that were never encrypted.
func TestPgAdapter_GetExistingCopiesForUpdate_ReportsUnencryptedCopy(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "plain")

	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		copies, err := a.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 1 {
			t.Fatalf("expected 1 copy, got %d", len(copies))
		}
		if copies[0].Encrypted || copies[0].HasDEK {
			t.Errorf("plain copy reported Encrypted=%v HasDEK=%v, want both false",
				copies[0].Encrypted, copies[0].HasDEK)
		}
	})
}

// TestPgAdapter_ImportObject_PreservesEncryptionMetadata verifies an import
// carrying encryption metadata writes every column, not just the four the
// adapter used to populate.
//
// This is the shape reconcile produces when it rediscovers an encrypted object
// on a backend: dropping the flag records ciphertext as plaintext, and
// replication then inherits that from the source row and spreads it.
func TestPgAdapter_ImportObject_PreservesEncryptionMetadata(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "enc-import")

	form := &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: []byte("packed-nonce-and-wrapped-dek"),
		KeyID:         "key-1",
		PlaintextSize: 1024,
		ContentHash:   "abc123",
	}
	inserted, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: "backend-a", Size: 1100, Form: form})
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()
	if inserted != core.ImportInserted {
		t.Fatalf("ImportObject outcome = %s, want inserted", inserted)
	}

	locs, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 copy, got %d", len(locs))
	}
	got := locs[0]

	if !got.Encrypted {
		t.Error("Encrypted = false, want true")
	}
	if !bytes.Equal(got.EncryptionKey, form.EncryptionKey) {
		t.Errorf("EncryptionKey = %q, want %q", got.EncryptionKey, form.EncryptionKey)
	}
	if got.KeyID != form.KeyID {
		t.Errorf("KeyID = %q, want %q", got.KeyID, form.KeyID)
	}
	if got.PlaintextSize != form.PlaintextSize {
		t.Errorf("PlaintextSize = %d, want %d", got.PlaintextSize, form.PlaintextSize)
	}
	if got.ContentHash != form.ContentHash {
		t.Errorf("ContentHash = %q, want %q", got.ContentHash, form.ContentHash)
	}
}

// TestPgAdapter_ImportObject_KeylessEncryptedRow verifies the exact row
// reconcile writes for an envelope whose key is gone: encrypted, but with no
// key to read it. Recording that as plaintext is what publishes ciphertext to
// clients as though it were the object.
func TestPgAdapter_ImportObject_KeylessEncryptedRow(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "unreadable")

	inserted, err := s.ImportObject(ctx, &core.ImportObjectRequest{
		Key: key, Backend: "backend-a", Size: 508, Form: &core.StoredForm{Encrypted: true},
	})
	if err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()
	if inserted != core.ImportInserted {
		t.Fatalf("ImportObject outcome = %s, want inserted", inserted)
	}

	locs, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 copy, got %d", len(locs))
	}
	if !locs[0].Encrypted {
		t.Error("Encrypted = false, want true - an envelope must never be recorded as plaintext")
	}
	if len(locs[0].EncryptionKey) != 0 {
		t.Errorf("EncryptionKey = %q, want empty", locs[0].EncryptionKey)
	}
}

// TestPgAdapter_ImportObject_PlaintextStaysPlaintext verifies an import with no
// encryption metadata is unaffected, so the fix does not start flagging
// ordinary objects.
func TestPgAdapter_ImportObject_PlaintextStaysPlaintext(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "plain")

	if _, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: "backend-a", Size: 100}); err != nil {
		t.Fatalf("ImportObject: %v", err)
	}
	defer func() { _, _ = s.DeleteObject(ctx, key) }()

	locs, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if locs[0].Encrypted || len(locs[0].EncryptionKey) != 0 {
		t.Errorf("plain import recorded Encrypted=%v key=%q, want false/empty",
			locs[0].Encrypted, locs[0].EncryptionKey)
	}
}
