// -------------------------------------------------------------------------------
// Integration Tests - Failable Store
//
// Author: Alex Freidah
//
// A metadata store that fails the calls a test picks. One fault decides every
// call it overrides: an outage fails all of them, and a narrower fault fails
// one method, or one method for one key, once or until it is cleared. Calls it
// does not override reach the real store untouched.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/postgres"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// errSimulatedDBOutage wraps core.ErrDBUnavailable, so degraded mode reacts to
// it as it does to an open database breaker.
var errSimulatedDBOutage = fmt.Errorf("simulated database outage: %w", core.ErrDBUnavailable)

// errSimulatedCommitFailure is a plain failure, so the caller fails the request
// instead of entering degraded mode.
var errSimulatedCommitFailure = errors.New("simulated commit failure")

// The store calls a fault can single out by name.
const (
	opRecordObject        = "RecordObject"
	opCommitCompanionCopy = "CommitCompanionCopy"
	opPromotePending      = "PromotePending"
	opDeleteObject        = "DeleteObject"
)

// storeFault decides one store call: op names the method and key the object it
// touches, empty when it touches none. A nil error lets the call through.
type storeFault func(op, key string) error

// FailableStore wraps the real store and fails the calls its fault picks. The
// embedded store answers every method this file does not override.
type FailableStore struct {
	storetest.MetadataStore
	inner *postgres.Store
	mu    sync.Mutex
	fault storeFault
}

// newFailableStore wraps db with no fault installed, so every call reaches it
// until a test installs one.
func newFailableStore(db *postgres.Store) *FailableStore {
	return &FailableStore{MetadataStore: db, inner: db}
}

// SetFault installs the fault every overridden call consults, replacing the one
// before it. A nil fault lets every call through.
func (f *FailableStore) SetFault(fault storeFault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fault = fault
}

// SetFailing starts or ends a simulated outage, during which every overridden
// call fails with errSimulatedDBOutage.
func (f *FailableStore) SetFailing(v bool) {
	if !v {
		f.SetFault(nil)
		return
	}
	f.SetFault(func(string, string) error { return errSimulatedDBOutage })
}

// SetFailCommitOnce fails the next RecordObject with errSimulatedCommitFailure,
// after the write's bytes and intent are in place, and lets later calls through.
func (f *FailableStore) SetFailCommitOnce() {
	f.SetFault(failOnce(opRecordObject, "", errSimulatedCommitFailure))
}

// failOnce fails the first call to op on key, or on any key when key is empty,
// and lets every later call through.
func failOnce(op, key string, err error) storeFault {
	var spent atomic.Bool
	return func(calledOp, calledKey string) error {
		if calledOp != op || (key != "" && calledKey != key) || !spent.CompareAndSwap(false, true) {
			return nil
		}
		return err
	}
}

// fail returns what the installed fault says about one call, nil when no fault
// is installed or it lets the call through.
func (f *FailableStore) fail(op, key string) error {
	f.mu.Lock()
	fault := f.fault
	f.mu.Unlock()
	if fault == nil {
		return nil
	}
	return fault(op, key)
}

// -------------------------------------------------------------------------
// OBJECTS
// -------------------------------------------------------------------------

// GetAllObjectLocations reads a key's copies unless the fault fails the
// call.
func (f *FailableStore) GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error) {
	if err := f.fail("GetAllObjectLocations", key); err != nil {
		return nil, err
	}
	return f.inner.GetAllObjectLocations(ctx, key)
}

// RecordObject records a write's copies unless the fault fails the call,
// which leaves the write's intents for the reaper.
func (f *FailableStore) RecordObject(ctx context.Context, req *core.RecordObjectRequest) ([]core.CleanupItem, error) {
	if err := f.fail(opRecordObject, req.Key); err != nil {
		return nil, err
	}
	return f.inner.RecordObject(ctx, req)
}

// CommitCompanionCopy adds a copy that landed after the client was answered
// unless the fault fails the call, which leaves its intent for the reaper.
func (f *FailableStore) CommitCompanionCopy(ctx context.Context, p *core.PendingObject) (core.CompanionCommitResult, []core.CleanupItem, error) {
	if err := f.fail(opCommitCompanionCopy, p.ObjectKey); err != nil {
		return core.CompanionCopyCommitted, nil, err
	}
	return f.inner.CommitCompanionCopy(ctx, p)
}

// PromotePending resolves an abandoned intent unless the fault fails the
// call, which leaves the intent for the reaper's next pass.
func (f *FailableStore) PromotePending(ctx context.Context, p *core.PendingObject) (core.PendingPromoteResult, []core.CleanupItem, error) {
	if err := f.fail(opPromotePending, p.ObjectKey); err != nil {
		return core.PendingPromoteAmbiguous, nil, err
	}
	return f.inner.PromotePending(ctx, p)
}

// DeleteObject removes a key's copies unless the fault fails the call.
// Nothing is queued for cleanup when it fails.
func (f *FailableStore) DeleteObject(ctx context.Context, key string) ([]core.CleanupItem, error) {
	if err := f.fail(opDeleteObject, key); err != nil {
		return nil, err
	}
	return f.inner.DeleteObject(ctx, key)
}

// ListObjects lists keys under a prefix unless the fault fails the
// call.
func (f *FailableStore) ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*core.ListObjectsResult, error) {
	if err := f.fail("ListObjects", prefix); err != nil {
		return nil, err
	}
	return f.inner.ListObjects(ctx, prefix, delimiter, startAfter, maxKeys)
}

// ListObjectsByBackend pages through one backend's copies unless the fault
// fails the call.
func (f *FailableStore) ListObjectsByBackend(ctx context.Context, backendName string, limit int, after core.SizeCursor) ([]core.ObjectLocation, error) {
	if err := f.fail("ListObjectsByBackend", ""); err != nil {
		return nil, err
	}
	return f.inner.ListObjectsByBackend(ctx, backendName, limit, after)
}

// PlaceCopy records a copy a worker placed unless the fault fails the
// call.
func (f *FailableStore) PlaceCopy(ctx context.Context, p *core.Placement) (int64, []core.CleanupItem, error) {
	if err := f.fail("PlaceCopy", ""); err != nil {
		return 0, nil, err
	}
	return f.inner.PlaceCopy(ctx, p)
}

// DeleteObjectLocation removes one copy unless the fault fails the
// call.
func (f *FailableStore) DeleteObjectLocation(ctx context.Context, loc *core.ObjectLocation, keepAtLeast int, reason string) ([]core.CleanupItem, error) {
	if err := f.fail("DeleteObjectLocation", loc.ObjectKey); err != nil {
		return nil, err
	}
	return f.inner.DeleteObjectLocation(ctx, loc, keepAtLeast, reason)
}

// -------------------------------------------------------------------------
// MULTIPART
// -------------------------------------------------------------------------

// CreateMultipartUpload opens an upload unless the fault fails the
// call.
func (f *FailableStore) CreateMultipartUpload(ctx context.Context, params *core.CreateMultipartUploadParams) (bool, error) {
	if err := f.fail("CreateMultipartUpload", ""); err != nil {
		return false, err
	}
	return f.inner.CreateMultipartUpload(ctx, params)
}

// GetMultipartUpload reads an upload unless the fault fails the
// call.
func (f *FailableStore) GetMultipartUpload(ctx context.Context, uploadID string) (*core.MultipartUpload, error) {
	if err := f.fail("GetMultipartUpload", ""); err != nil {
		return nil, err
	}
	return f.inner.GetMultipartUpload(ctx, uploadID)
}

// RecordPart records an uploaded part unless the fault fails the
// call.
func (f *FailableStore) RecordPart(ctx context.Context, p *core.RecordPartParams) ([]core.CleanupItem, error) {
	if err := f.fail("RecordPart", ""); err != nil {
		return nil, err
	}
	return f.inner.RecordPart(ctx, p)
}

// ListParts pages through an upload's parts unless the fault fails the
// call.
func (f *FailableStore) ListParts(ctx context.Context, uploadID string, afterPart, limit int) ([]core.MultipartPart, error) {
	if err := f.fail("ListParts", ""); err != nil {
		return nil, err
	}
	return f.inner.ListParts(ctx, uploadID, afterPart, limit)
}

// DeleteMultipartUpload removes an upload and its parts unless the fault
// fails the call.
func (f *FailableStore) DeleteMultipartUpload(ctx context.Context, uploadID, reason string, staleBefore time.Time) ([]core.CleanupItem, error) {
	if err := f.fail("DeleteMultipartUpload", ""); err != nil {
		return nil, err
	}
	return f.inner.DeleteMultipartUpload(ctx, uploadID, reason, staleBefore)
}

// BeginMultipartCompletion takes an upload's completion mark unless the
// fault fails the call.
func (f *FailableStore) BeginMultipartCompletion(ctx context.Context, uploadID string, staleBefore time.Time) (bool, error) {
	if err := f.fail("BeginMultipartCompletion", ""); err != nil {
		return false, err
	}
	return f.inner.BeginMultipartCompletion(ctx, uploadID, staleBefore)
}

// RenewMultipartCompletion extends an upload's completion mark unless the
// fault fails the call.
func (f *FailableStore) RenewMultipartCompletion(ctx context.Context, uploadID string) (bool, error) {
	if err := f.fail("RenewMultipartCompletion", ""); err != nil {
		return false, err
	}
	return f.inner.RenewMultipartCompletion(ctx, uploadID)
}

// EndMultipartCompletion releases an upload's completion mark unless the
// fault fails the call.
func (f *FailableStore) EndMultipartCompletion(ctx context.Context, uploadID string) error {
	if err := f.fail("EndMultipartCompletion", ""); err != nil {
		return err
	}
	return f.inner.EndMultipartCompletion(ctx, uploadID)
}

// GetActiveMultipartCounts counts open uploads per backend unless the fault
// fails the call.
func (f *FailableStore) GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error) {
	if err := f.fail("GetActiveMultipartCounts", ""); err != nil {
		return nil, err
	}
	return f.inner.GetActiveMultipartCounts(ctx)
}

// ScanMultipartUploads pages through uploads matching a filter unless the
// fault fails the call.
func (f *FailableStore) ScanMultipartUploads(ctx context.Context, filter core.MultipartUploadFilter, limit int, afterUploadID string) ([]core.MultipartUpload, error) {
	if err := f.fail("ScanMultipartUploads", ""); err != nil {
		return nil, err
	}
	return f.inner.ScanMultipartUploads(ctx, filter, limit, afterUploadID)
}

// -------------------------------------------------------------------------
// FLEET FIGURES
// -------------------------------------------------------------------------

// GetQuotaStats reads every backend's quota unless the fault fails the
// call.
func (f *FailableStore) GetQuotaStats(ctx context.Context) (map[string]core.QuotaStat, error) {
	if err := f.fail("GetQuotaStats", ""); err != nil {
		return nil, err
	}
	return f.inner.GetQuotaStats(ctx)
}

// LedgerStats reads the ledger's fleet figures unless the fault fails the
// call.
func (f *FailableStore) LedgerStats(ctx context.Context) (core.LedgerStats, error) {
	if err := f.fail("LedgerStats", ""); err != nil {
		return nil, err
	}
	return f.inner.LedgerStats(ctx)
}

// RebuildReplicationQueue queues every key off the factor unless the fault
// fails the call.
func (f *FailableStore) RebuildReplicationQueue(ctx context.Context, factor int, excluded []string) (int64, error) {
	if err := f.fail("RebuildReplicationQueue", ""); err != nil {
		return 0, err
	}
	return f.inner.RebuildReplicationQueue(ctx, factor, excluded)
}

// ListReplicationQueue reads a side's due keys unless the fault fails the
// call.
func (f *FailableStore) ListReplicationQueue(ctx context.Context, direction core.ReplicationDirection, limit int, excluded []string) ([]core.QueuedReplication, error) {
	if err := f.fail("ListReplicationQueue", ""); err != nil {
		return nil, err
	}
	return f.inner.ListReplicationQueue(ctx, direction, limit, excluded)
}

// CountReplicationQueue counts the queued keys unless the fault fails the
// call.
func (f *FailableStore) CountReplicationQueue(ctx context.Context) (core.ReplicationBacklog, error) {
	if err := f.fail("CountReplicationQueue", ""); err != nil {
		return core.ReplicationBacklog{}, err
	}
	return f.inner.CountReplicationQueue(ctx)
}
