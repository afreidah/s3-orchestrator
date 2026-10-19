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
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	db "github.com/afreidah/s3-orchestrator/internal/store/postgres/sqlc"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// -------------------------------------------------------------------------
// SHARED CONTAINER FIXTURE
// -------------------------------------------------------------------------

// pgFixtureInstance is the instance id the shared store stamps on the cleanup
// rows its transactions queue.
const pgFixtureInstance = "pg-fixture"

// The lazily started Postgres testcontainer shared by the whole suite.
var (
	pgFixtureOnce sync.Once
	pgFixture     *Store
	pgFixtureErr  error
)

// adapterPgStore lazily starts one Postgres testcontainer for the suite,
// applies the migrations, seeds quota rows for the test backends, and returns
// the shared *Store; Ryuk tears the container down. The pool is sized for the
// parallel benchmarks, so they measure row contention rather than waiting on
// connections.
func adapterPgStore(tb testing.TB) *Store {
	tb.Helper()
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
			MaxConns: 16,
			MinConns: 1,
		}, nil, pgFixtureInstance)
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
		tb.Fatalf("postgres fixture: %v", pgFixtureErr)
	}
	if pgFixture == nil {
		tb.Fatal("postgres fixture nil")
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
	fn(&pgTxAdapter{q: s.queries.WithTx(tx), instanceID: s.instanceID})
}

// uniqueKey returns a key prefixed with the test name so suites can
// run in parallel against the shared container without colliding on
// rows committed outside the rollback-scoped adapter tx.
func uniqueKey(t *testing.T, suffix string) string {
	t.Helper()
	return t.Name() + "/" + suffix
}

// claimCopyIntent claims target for a copy of key written at storageKey, the
// way a copy's caller claims its target before writing, and fails the test
// unless the backend admits the bytes.
func claimCopyIntent(t *testing.T, s *Store, key, target, storageKey string, size int64) *core.PendingObject {
	t.Helper()
	p := &core.PendingObject{
		IntentID: storageKey + "@" + target, ObjectKey: key, StorageKey: storageKey,
		BackendName: target, SizeBytes: size, Role: core.PendingRoleCompanion,
	}
	fits, err := s.InsertPendingIfFits(context.Background(), p, core.AcceptForWrite)
	if err != nil || !fits {
		t.Fatalf("InsertPendingIfFits(%s on %s): fits=%v err=%v", storageKey, target, fits, err)
	}
	return p
}

// replicaOf describes a replica of the copy of key on source placed onto
// target, stored at a path of its own under an intent claimed for it, the way
// the replicator places one.
func replicaOf(t *testing.T, s *Store, key, target, source string) *core.Placement {
	t.Helper()
	src := storetest.LocationOn(t, s, key, source)
	return &core.Placement{Source: src, Intent: claimCopyIntent(t, s, key, target, key+"!r", src.SizeBytes)}
}

// mustRecordReplica places a replica of the copy of key on source onto
// target, failing the test on error.
func mustRecordReplica(t *testing.T, s *Store, key, target, source string) {
	t.Helper()
	if _, _, err := s.PlaceCopy(context.Background(), replicaOf(t, s, key, target, source)); err != nil {
		t.Fatalf("PlaceCopy(%s, %s): %v", key, target, err)
	}
}

// cleanupOf describes a cleanup row for bytes stored at the object's key.
func cleanupOf(backend, key, reason string, size int64) *core.CleanupRequest {
	return &core.CleanupRequest{BackendName: backend, ObjectKey: key, StorageKey: key, Reason: reason, SizeBytes: size}
}

// queueCleanup commits a cleanup row the way a copy-removing transaction
// queues one, claimed by the fixture's instance, and returns the row with its
// claim.
func queueCleanup(t *testing.T, s *Store, c *core.CleanupRequest) core.CleanupItem {
	t.Helper()
	item, err := s.direct().InsertCleanup(context.Background(), c)
	if err != nil {
		t.Fatalf("InsertCleanup(%s): %v", c.ObjectKey, err)
	}
	return item
}

// dropCleanupRows deletes the queued rows with ids whatever claim holds them,
// so a test does not leave rows behind in the shared fixture.
func dropCleanupRows(t *testing.T, s *Store, ids ...int64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM cleanup_queue WHERE id = ANY($1)`, ids); err != nil {
		t.Fatalf("drop cleanup rows %v: %v", ids, err)
	}
}

// releaseClaim clears a queued row's claim, as a retry does, so the next
// claim takes it as a fresh row.
func releaseClaim(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE cleanup_queue SET claimed_at = NULL, claimed_by = NULL WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("release claim %d: %v", id, err)
	}
}

// queueUnclaimed commits a cleanup row with no claim on it.
func queueUnclaimed(t *testing.T, s *Store, c *core.CleanupRequest) int64 {
	t.Helper()
	id := queueCleanup(t, s, c).ID
	releaseClaim(t, s, id)
	return id
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
	if _, err := s.InsertPendingIfFits(ctx, &core.PendingObject{
		IntentID: intentID, ObjectKey: uniqueKey(t, "k"), BackendName: "backend-a", SizeBytes: 1,
	}, core.AcceptForWrite); err != nil {
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
	if _, err := s.InsertPendingIfFits(ctx, &want, core.AcceptForWrite); err != nil {
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
	if _, err := s.InsertPendingIfFits(ctx, &core.PendingObject{
		IntentID: intentID, ObjectKey: uniqueKey(t, "k"), BackendName: "backend-a", SizeBytes: 5,
	}, core.AcceptForWrite); err != nil {
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
	if _, err := s.InsertPendingIfFits(ctx, &core.PendingObject{
		IntentID: intentID, ObjectKey: uniqueKey(t, "k"), BackendName: "backend-a", SizeBytes: 1,
	}, core.AcceptForWrite); err != nil {
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
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()
	mustRecordReplica(t, s, key, "backend-b", "backend-a")

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
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 75, Form: form}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

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
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject(target): %v", err)
	}
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: other, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject(other): %v", err)
	}
	defer func() {
		_, _, _ = s.DeleteObject(ctx, key)
		_, _, _ = s.DeleteObject(ctx, other)
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
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 75, Form: form}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

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
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	mustRecordReplica(t, s, key, "backend-b", "backend-a")
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

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
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

	if got, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: "backend-a", Size: 50}); err != nil || got != core.ImportInserted {
		t.Fatalf("ImportObject(insert): got=%s err=%v, want inserted", got, err)
	}
	if got, err := s.ImportObject(ctx, &core.ImportObjectRequest{Key: key, Backend: "backend-a", Size: 50}); err != nil || got != core.ImportSkippedExisting {
		t.Errorf("ImportObject(idempotent): got=%s err=%v, want skipped_existing", got, err)
	}
}

// -------------------------------------------------------------------------
// CLEANUP TX
// -------------------------------------------------------------------------

// txCleanupRow reads the queued row with id inside the adapter's transaction,
// failing the test when the row is not there.
func txCleanupRow(t *testing.T, a *pgTxAdapter, id int64) core.CleanupItem {
	t.Helper()
	rows, err := a.q.GetPendingCleanups(context.Background(), db.GetPendingCleanupsParams{
		MaxAttempts: core.MaxCleanupAttempts, RowLimit: math.MaxInt32,
	})
	if err != nil {
		t.Fatalf("GetPendingCleanups: %v", err)
	}
	for i := range rows {
		if rows[i].ID == id {
			return cleanupItemFromRow(&rows[i])
		}
	}
	t.Fatalf("cleanup row %d not queued", id)
	return core.CleanupItem{}
}

// TestPgAdapter_InsertCleanup_ClaimsRowForInstance verifies a queued row is
// stamped with the adapter's instance and a claim time, stored at the copy's
// own path, and returned with the claim the row holds.
func TestPgAdapter_InsertCleanup_ClaimsRowForInstance(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		item, err := a.InsertCleanup(ctx, &core.CleanupRequest{
			BackendName: "backend-a", ObjectKey: key, StorageKey: key + "!w1", Reason: "test", SizeBytes: 256,
		})
		if err != nil {
			t.Fatalf("InsertCleanup: %v", err)
		}
		row := txCleanupRow(t, a, item.ID)
		if row.StorageKey != key+"!w1" || row.ObjectKey != key || row.SizeBytes != 256 || row.Reason != "test" {
			t.Errorf("row = %+v, want the queued request's fields", row)
		}
		if item.Claim == nil || row.Claim == nil {
			t.Fatalf("claims = returned %+v, stored %+v; want both set", item.Claim, row.Claim)
		}
		if !item.Claim.At.Equal(row.Claim.At) || item.Claim.By != pgFixtureInstance || row.Claim.By != pgFixtureInstance {
			t.Errorf("returned claim %+v, stored claim %+v; want the same stamp by %q", *item.Claim, *row.Claim, pgFixtureInstance)
		}
		if item.StorageKey != row.StorageKey || item.BackendName != row.BackendName || item.SizeBytes != row.SizeBytes {
			t.Errorf("returned item %+v, want the stored row's fields %+v", item, row)
		}
	})
}

// TestPgAdapter_InsertCleanup_NoInstanceClaimsWithEmptyBy verifies a process
// with no instance id still stamps a claim time, and the claim it returns
// has an empty By.
func TestPgAdapter_InsertCleanup_NoInstanceClaimsWithEmptyBy(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a := &pgTxAdapter{q: s.queries.WithTx(tx)}
	item, err := a.InsertCleanup(ctx, cleanupOf("backend-a", uniqueKey(t, "k"), "test", 1))
	if err != nil {
		t.Fatalf("InsertCleanup: %v", err)
	}
	row := txCleanupRow(t, a, item.ID)
	if item.Claim == nil || item.Claim.By != "" || item.Claim.At.IsZero() {
		t.Fatalf("claim = %+v, want a stamped claim with empty By", item.Claim)
	}
	if row.Claim == nil || row.Claim.By != "" || !row.Claim.At.Equal(item.Claim.At) {
		t.Errorf("stored claim = %+v, want %+v", row.Claim, *item.Claim)
	}
}

// TestPgAdapter_InsertCleanup_EmptyStorageKeyUsesObjectKey verifies a copy
// stored at the object's own key is queued at that key.
func TestPgAdapter_InsertCleanup_EmptyStorageKeyUsesObjectKey(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	key := uniqueKey(t, "k")
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		item, err := a.InsertCleanup(ctx, &core.CleanupRequest{BackendName: "backend-a", ObjectKey: key, Reason: "test", SizeBytes: 1})
		if err != nil {
			t.Fatalf("InsertCleanup: %v", err)
		}
		if row := txCleanupRow(t, a, item.ID); row.StorageKey != key || item.StorageKey != key {
			t.Errorf("StorageKey = stored %q, returned %q; want the object key %q", row.StorageKey, item.StorageKey, key)
		}
	})
}

// -------------------------------------------------------------------------
// MULTIPART TX
// -------------------------------------------------------------------------

// partParams describes part n of uploadID written at storageKey.
func partParams(uploadID string, n int, storageKey, etag string, size int64) *core.RecordPartParams {
	return &core.RecordPartParams{
		UploadID: uploadID, PartNumber: n, ETag: etag, SizeBytes: size,
		Intent: &core.PendingObject{BackendName: "backend-a", StorageKey: storageKey},
	}
}

// TestPgAdapter_UpsertPart_ReturnsReplacedPart verifies the first upload of a
// part number replaces nothing, and a re-upload returns the part it replaced
// with that part's path while the new path is stored.
func TestPgAdapter_UpsertPart_ReturnsReplacedPart(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	uploadID, _ := seedMultipartUpload(t, s, "", nil)
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		replaced, err := a.UpsertPart(ctx, partParams(uploadID, 1, "path-1", "e1", 100))
		if err != nil || replaced != nil {
			t.Fatalf("UpsertPart(first) = %+v, %v; want nil, nil", replaced, err)
		}
		replaced, err = a.UpsertPart(ctx, partParams(uploadID, 1, "path-2", "e2", 200))
		if err != nil {
			t.Fatalf("UpsertPart(second): %v", err)
		}
		if replaced == nil || replaced.StorageKey != "path-1" || replaced.SizeBytes != 100 || replaced.PartNumber != 1 {
			t.Errorf("replaced = %+v, want part 1 at path-1 with 100 bytes", replaced)
		}
	})
}

// TestPgAdapter_UpsertPart_StoresStorageKey verifies a recorded part reads back
// with the path its upload wrote.
func TestPgAdapter_UpsertPart_StoresStorageKey(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	uploadID, _ := seedMultipartUpload(t, s, "", nil)
	if err := s.WithTx(ctx, func(ctx context.Context, tx core.TxAdapter) error {
		_, err := tx.UpsertPart(ctx, partParams(uploadID, 3, "part-path", "e", 64))
		return err
	}); err != nil {
		t.Fatalf("UpsertPart: %v", err)
	}
	parts, err := s.GetParts(ctx, uploadID)
	if err != nil {
		t.Fatalf("GetParts: %v", err)
	}
	if len(parts) != 1 || parts[0].StorageKey != "part-path" {
		t.Fatalf("parts = %+v, want one part at part-path", parts)
	}
	if got := parts[0].Path(uploadID); got != "part-path" {
		t.Errorf("Path = %q, want part-path", got)
	}
}

// TestPgAdapter_DeleteMultipartUpload_ReturnsBackendAndParts verifies removing
// an upload returns its backend and every part with its path, and leaves no
// upload or part rows behind.
func TestPgAdapter_DeleteMultipartUpload_ReturnsBackendAndParts(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	uploadID, _ := seedMultipartUpload(t, s, "", nil)
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		for n, path := range map[int]string{1: "p1", 2: ""} {
			if _, err := a.UpsertPart(ctx, partParams(uploadID, n, path, "e", int64(n*10))); err != nil {
				t.Fatalf("UpsertPart(%d): %v", n, err)
			}
		}
		backend, parts, err := a.DeleteMultipartUpload(ctx, uploadID)
		if err != nil {
			t.Fatalf("DeleteMultipartUpload: %v", err)
		}
		if backend != "backend-a" {
			t.Errorf("backend = %q, want backend-a", backend)
		}
		assertRemovedParts(t, parts)
		if again, left, err := a.DeleteMultipartUpload(ctx, uploadID); err != nil || again != "" || left != nil {
			t.Errorf("second delete = %q, %+v, %v; want nothing", again, left, err)
		}
	})
}

// assertRemovedParts checks the removed parts are part 1 at p1 and part 2 at
// its fixed path, each sized ten bytes per part number.
func assertRemovedParts(t *testing.T, parts []core.MultipartPart) {
	t.Helper()
	if len(parts) != 2 {
		t.Fatalf("removed %d parts, want 2", len(parts))
	}
	keys := map[int]string{}
	for i := range parts {
		p := &parts[i]
		keys[p.PartNumber] = p.StorageKey
		if p.SizeBytes != int64(p.PartNumber*10) {
			t.Errorf("part %d size = %d, want %d", p.PartNumber, p.SizeBytes, p.PartNumber*10)
		}
	}
	if keys[1] != "p1" || keys[2] != "" {
		t.Errorf("storage keys = %v, want 1:p1 and 2 empty", keys)
	}
}

// TestPgAdapter_DeleteMultipartUpload_MissingUploadReturnsNothing verifies an
// upload already gone returns no backend and no parts.
func TestPgAdapter_DeleteMultipartUpload_MissingUploadReturnsNothing(t *testing.T) {
	s := adapterPgStore(t)
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		backend, parts, err := a.DeleteMultipartUpload(context.Background(), uniqueKey(t, "missing"))
		if err != nil || backend != "" || parts != nil {
			t.Errorf("DeleteMultipartUpload = %q, %+v, %v; want empty", backend, parts, err)
		}
	})
}

// -------------------------------------------------------------------------
// QUOTA TX
// -------------------------------------------------------------------------

// TestPgAdapter_AdjustQuotaStripe_MaterializesAndAccumulates asserts the first
// adjustment creates the stripe row and later ones add to it, so nothing has to
// seed a backend's stripes before it can be charged.
func TestPgAdapter_AdjustQuotaStripe_MaterializesAndAccumulates(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		before := pgBytesUsed(t, ctx, a, "backend-a")
		if err := a.AdjustQuotaStripe(ctx, "backend-a", 4, 500); err != nil {
			t.Fatalf("AdjustQuotaStripe: %v", err)
		}
		if err := a.AdjustQuotaStripe(ctx, "backend-a", 4, 250); err != nil {
			t.Fatalf("AdjustQuotaStripe: %v", err)
		}
		if got := pgBytesUsed(t, ctx, a, "backend-a") - before; got != 750 {
			t.Errorf("total moved by %d, want 750 across two charges on one stripe", got)
		}
	})
}

// pgBytesUsed reads a backend's striped byte total through the adapter.
func pgBytesUsed(t *testing.T, ctx context.Context, a *pgTxAdapter, backend string) int64 {
	t.Helper()
	totals, err := a.AllBackendBytesUsed(ctx)
	if err != nil {
		t.Fatalf("AllBackendBytesUsed: %v", err)
	}
	return totals[backend]
}

// TestPgAdapter_AdjustQuotaStripe_NegativeStripeIsHeldUnclamped asserts a
// stripe may sit negative. A credit for an object recorded before the stripes
// existed lands on whichever stripe its key hashes to rather than the one
// holding the backfilled bytes, so clamping per stripe would discard the debit
// and leave the total permanently high.
func TestPgAdapter_AdjustQuotaStripe_NegativeStripeIsHeldUnclamped(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	withPgAdapter(t, s, func(a *pgTxAdapter) {
		if err := a.AdjustQuotaStripe(ctx, "backend-a", 8, 1000); err != nil {
			t.Fatalf("seed stripe 8: %v", err)
		}
		before := pgBytesUsed(t, ctx, a, "backend-a")
		if err := a.AdjustQuotaStripe(ctx, "backend-a", 9, -400); err != nil {
			t.Fatalf("credit stripe 9: %v", err)
		}
		// A stripe clamped at zero would leave the total unchanged.
		if got := pgBytesUsed(t, ctx, a, "backend-a") - before; got != -400 {
			t.Errorf("total moved by %d, want -400 from a stripe allowed to go negative", got)
		}
	})
}

// TestPgAdapter_GetExistingCopiesForUpdate_CarriesEncryptionState verifies the
// Postgres locked re-read reports the same encryption state the SQLite adapter
// does. The surplus-copy removal's guard reads that form, so the engines must
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
	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 1100, Form: form}); err != nil {
		t.Fatalf("RecordObject encrypted: %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()
	mustRecordReplica(t, s, key, "backend-b", "backend-a")

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		copies, err := a.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 2 {
			t.Fatalf("expected 2 copies, got %d", len(copies))
		}
		for _, ec := range copies {
			if ec.Form != core.StoredReadable {
				t.Errorf("%s: Form = %s, want encrypted,keyed (replication copies the key)", ec.BackendName, ec.Form)
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

	if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 100}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

	withPgAdapter(t, s, func(a *pgTxAdapter) {
		copies, err := a.GetExistingCopiesForUpdate(ctx, key)
		if err != nil {
			t.Fatalf("GetExistingCopiesForUpdate: %v", err)
		}
		if len(copies) != 1 {
			t.Fatalf("expected 1 copy, got %d", len(copies))
		}
		if copies[0].Form != 0 {
			t.Errorf("plain copy reported Form = %s, want no flags", copies[0].Form)
		}
	})
}

// TestPgAdapter_ImportObject_PreservesEncryptionMetadata verifies an import
// carrying encryption metadata writes every encryption column. Reconcile
// imports rediscovered encrypted objects this way, and a dropped flag would
// record ciphertext as plaintext and replicate that mistake.
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
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()
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
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()
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
	if !locs[0].Unmanaged {
		t.Error("Unmanaged = false, want true - an unreadable row must not be listed, served or replicated")
	}
	assertUnreadableHidden(t, s, key)
}

// assertUnreadableHidden fails unless key is left out of both client listings
// and returned by the unreadable list and count.
func assertUnreadableHidden(t *testing.T, s *Store, key string) {
	t.Helper()
	ctx := context.Background()
	flat, err := s.ListObjects(ctx, key, "", 10)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(flat.Objects) != 0 {
		t.Errorf("ListObjects = %+v, want the unreadable row left out", flat.Objects)
	}
	delimited, err := s.ListObjectsDelimited(ctx, key, "/", "", 10)
	if err != nil {
		t.Fatalf("ListObjectsDelimited: %v", err)
	}
	if len(delimited.Objects) != 0 || len(delimited.CommonPrefixes) != 0 {
		t.Errorf("ListObjectsDelimited = %+v, want the unreadable row left out", delimited)
	}

	unreadable, err := s.ListCopiesByForm(ctx, &unreadableCopies, 10000, core.Cursor{})
	if err != nil {
		t.Fatalf("ListCopiesByForm(unreadable): %v", err)
	}
	if !slices.ContainsFunc(unreadable, func(l core.ObjectLocation) bool { return l.ObjectKey == key }) {
		t.Errorf("the unreadable listing did not return %s", key)
	}
	if n, err := s.CountCopiesByForm(ctx, unreadableCopies.With, unreadableCopies.Without); err != nil || n < 1 {
		t.Errorf("the unreadable count = %d, %v; want at least 1", n, err)
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
	defer func() { _, _, _ = s.DeleteObject(ctx, key) }()

	locs, err := s.GetAllObjectLocations(ctx, key)
	if err != nil {
		t.Fatalf("GetAllObjectLocations: %v", err)
	}
	if locs[0].Encrypted || len(locs[0].EncryptionKey) != 0 {
		t.Errorf("plain import recorded Encrypted=%v key=%q, want false/empty",
			locs[0].Encrypted, locs[0].EncryptionKey)
	}
}
