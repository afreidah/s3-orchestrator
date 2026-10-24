// -------------------------------------------------------------------------------
// Write Coordinator - Branch Tests
//
// Author: Alex Freidah
//
// Targeted tests for the branches of Coordinator that the PUT/multipart
// end-to-end tests do not exercise: intent construction, claiming write and
// copy targets, settling the cleanup rows a transaction hands back, discarding
// unrecorded uploads, and the claim/copy/place sequence of a move.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/mock/gomock"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
)

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newCoordinatorWithBackend builds a Coordinator whose infra.BackendRuntime
// knows about a single named backend, so the backend's deletes can be driven
// through the gomock recorder. A real in-memory UsageTracker keeps the
// accounting calls from nil-dereferencing.
func newCoordinatorWithBackend(name string, be s3be.ObjectBackend, store CoordinatorStores) *Coordinator {
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{name}), nil)
	c := infra.New(&infra.Config{
		Backends: map[string]s3be.ObjectBackend{name: be},
		Usage:    usage,
		Quota:    counter.NewQuotaTracker(),
	})
	return New(c, store)
}

// newCoordinatorWithStore builds a minimal Coordinator backed by the
// supplied store. Avoids the live-fleet fixture so coordinator branches
// can be exercised in isolation without dragging real backends into
// every test.
func newCoordinatorWithStore(store CoordinatorStores) *Coordinator {
	c := infra.New(&infra.Config{
		Backends: map[string]s3be.ObjectBackend{},
		Usage:    counter.NewUsageTracker(counter.NewLocalCounterBackend(nil), nil),
		Quota:    counter.NewQuotaTracker(),
	})
	return New(c, store)
}

// newCoordinatorWith2Backends builds a Coordinator that knows a source and a
// destination backend, so MoveObject's StreamCopy (GetObject src -> PutObject
// dest) and its cleanup paths can be driven through the gomock recorders.
func newCoordinatorWith2Backends(srcName string, src s3be.ObjectBackend, destName string, dest s3be.ObjectBackend, store CoordinatorStores) *Coordinator {
	coord, _ := newMoveCoordinator(srcName, src, destName, dest, store)
	return coord
}

// loadUsage returns a backend's unflushed counters as a fresh value, for the
// assertions that read them.
func loadUsage(b counter.Backend, name string) core.Usage {
	var u core.Usage
	b.Load(name, time.Time{}, &u)
	return u
}

// newMoveCoordinator is newCoordinatorWith2Backends that also returns the
// usage counters the copy is charged to.
func newMoveCoordinator(srcName string, src s3be.ObjectBackend, destName string, dest s3be.ObjectBackend, store CoordinatorStores) (*Coordinator, *counter.LocalCounterBackend) {
	counters := counter.NewLocalCounterBackend([]string{srcName, destName})
	c := infra.New(&infra.Config{
		Backends:       map[string]s3be.ObjectBackend{srcName: src, destName: dest},
		Order:          []string{srcName, destName},
		BackendTimeout: 5 * time.Second,
		Usage:          counter.NewUsageTracker(counters, nil),
		Quota:          counter.NewQuotaTracker(),
	})
	return New(c, store), counters
}

// expectStreamCopyOK wires the happy StreamCopy: read 4 bytes from src, write
// them to dest. Returns the path the move wrote on dest, set once the copy
// runs, and the source read so a test can order the claim before it.
func expectStreamCopyOK(src, dest *backendtest.MockObjectBackend) (*string, *gomock.Call) {
	written := new(string)
	get := src.EXPECT().GetObject(gomock.Any(), srcPath, "").
		Return(&s3be.GetObjectResult{Body: io.NopCloser(strings.NewReader("data")), Size: 4}, nil)
	dest.EXPECT().PutObject(gomock.Any(), gomock.Any(), gomock.Any(), int64(4), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, key string, _ io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
			*written = key
			return "etag", nil
		})
	return written, get
}

// srcPath is where the source copy sits. The move writes its own copy under a
// freshly minted path, so each cleanup path deletes exactly its own bytes.
const srcPath = "k!src-copy"

// sourceName and destName are the backends the move reads from and writes to,
// named once so the expectations and the request cannot drift apart.
const (
	sourceName = "src"
	destName   = "dest"
)

// errDBDown is the store failure the error-path tests inject.
var errDBDown = errors.New("db down")

// -------------------------------------------------------------------------
// MATCHERS AND BUILDERS
// -------------------------------------------------------------------------

// moveReq builds the standard rebalance MoveRequest the MoveObject tests share,
// from sourceName to destName.
func moveReq() *MoveRequest {
	return &MoveRequest{
		Source:   &core.ObjectLocation{ObjectKey: "k", BackendName: sourceName, StorageKey: srcPath, SizeBytes: 4},
		DestName: destName,
		Profile:  RebalanceMove,
	}
}

// rowClaim is the claim every cleanupRow is queued under, so the settle paths
// can show the claim reaches the store unchanged.
var rowClaim = &core.CleanupClaim{At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), By: "inst-1"}

// cleanupRow builds one queued cleanup row for the settle paths, held under
// rowClaim.
func cleanupRow(id int64, backendName, path, reason string, size int64) core.CleanupItem {
	return core.CleanupItem{ID: id, BackendName: backendName, ObjectKey: "k", StorageKey: path, Reason: reason, SizeBytes: size, Claim: rowClaim}
}

// retiring matches one CompleteCleanupItems batch holding exactly the rows
// with ids, in order, each still carrying the claim it was queued under.
func retiring(ids ...int64) gomock.Matcher {
	return gomock.Cond(func(items []core.CleanupItem) bool {
		if len(items) != len(ids) {
			return false
		}
		for i := range items {
			if items[i].ID != ids[i] || items[i].Claim != rowClaim {
				return false
			}
		}
		return true
	})
}

// retireAll answers CompleteCleanupItems by retiring every row it was handed,
// as the store does when no other instance took a claim over.
func retireAll(_ context.Context, items []core.CleanupItem) ([]int64, error) {
	ids := make([]int64, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	return ids, nil
}

// copyIntentOn matches the companion intent a move claims on target for size
// bytes.
func copyIntentOn(target string, size int64) gomock.Matcher {
	return gomock.Cond(func(p *core.PendingObject) bool {
		return p.ObjectKey == "k" && p.BackendName == target && p.SizeBytes == size &&
			p.Role == core.PendingRoleCompanion && p.IntentID != "" && p.StorageKey != srcPath
	})
}

// placementOf matches the move's placement: the source copy moveReq read,
// removed as its intent's copy lands on dest at the path the copy wrote.
func placementOf(written *string, reasons core.MoveReasons) gomock.Matcher {
	return gomock.Cond(func(p *core.Placement) bool {
		return p.Source.ObjectKey == "k" && p.Source.BackendName == sourceName &&
			p.Source.StorageKey == srcPath && p.Intent.BackendName == destName &&
			*written != "" && *written != srcPath && p.Intent.StorageKey == *written &&
			p.RemoveSource && !p.Reformed && p.Reasons == reasons
	})
}

// -------------------------------------------------------------------------
// MOVE
// -------------------------------------------------------------------------

// TestMoveObject_ClaimsThenCopiesThenPlaces pins the success path: the
// destination is claimed before the source is read, the placement carries
// the claimed intent, and the source cleanup the placement queued is settled.
func TestMoveObject_ClaimsThenCopiesThenPlaces(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src := backendtest.NewMockObjectBackend(ctrl)
	dest := backendtest.NewMockObjectBackend(ctrl)
	written, get := expectStreamCopyOK(src, dest)
	src.EXPECT().DeleteObject(gomock.Any(), srcPath).Return(nil)

	store := NewMockCoordinatorStores(ctrl)
	claim := store.EXPECT().InsertPendingIfFits(gomock.Any(), copyIntentOn(destName, 4), core.AcceptForWrite).Return(true, nil)
	gomock.InOrder(claim, get)
	store.EXPECT().PlaceCopy(gomock.Any(), placementOf(written, RebalanceMove.Reasons)).
		Return(int64(4), []core.CleanupItem{cleanupRow(11, sourceName, srcPath, RebalanceMove.Reasons.SourceDelete, 4)}, nil)
	store.EXPECT().CompleteCleanupItems(gomock.Any(), retiring(11)).DoAndReturn(retireAll)

	coord, counters := newMoveCoordinator(sourceName, src, destName, dest, store)
	movedSize, err := coord.MoveObject(context.Background(), moveReq())
	if err != nil {
		t.Fatalf("MoveObject: %v", err)
	}
	if movedSize != 4 {
		t.Errorf("movedSize = %d, want 4", movedSize)
	}
	if got := loadUsage(counters, sourceName).EgressBytes; got != 4 {
		t.Errorf("source egress = %d, want the 4 bytes read once", got)
	}
	if got := loadUsage(counters, destName).IngressBytes; got != 4 {
		t.Errorf("dest ingress = %d, want the 4 bytes written once", got)
	}
}

// TestMoveObject_ClaimRefusedCopiesNothing verifies a destination without room
// ends the move before any byte is read, and the refusal reads as declined.
func TestMoveObject_ClaimRefusedCopiesNothing(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src := backendtest.NewMockObjectBackend(ctrl)
	dest := backendtest.NewMockObjectBackend(ctrl)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), core.AcceptForWrite).Return(false, nil)

	coord := newCoordinatorWith2Backends(sourceName, src, destName, dest, store)
	_, err := coord.MoveObject(context.Background(), moveReq())
	if !errors.Is(err, core.ErrTargetRefused) || !core.PlacementDeclined(err) {
		t.Fatalf("err = %v, want a declined placement wrapping ErrTargetRefused", err)
	}
}

// TestMoveObject_ClaimErrorCopiesNothing verifies a database error on the claim
// ends the move and is not read as a declined placement.
func TestMoveObject_ClaimErrorCopiesNothing(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src := backendtest.NewMockObjectBackend(ctrl)
	dest := backendtest.NewMockObjectBackend(ctrl)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, errDBDown)

	coord := newCoordinatorWith2Backends(sourceName, src, destName, dest, store)
	_, err := coord.MoveObject(context.Background(), moveReq())
	if !errors.Is(err, errDBDown) || core.PlacementDeclined(err) {
		t.Fatalf("err = %v, want the claim failure, not a declined placement", err)
	}
}

// TestMoveObject_CopyFailureDiscardsTheClaim verifies a failed copy discards the
// claimed intent under the profile's Orphan reason and settles what the
// discard queued.
func TestMoveObject_CopyFailureDiscardsTheClaim(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src := backendtest.NewMockObjectBackend(ctrl)
	dest := backendtest.NewMockObjectBackend(ctrl)
	src.EXPECT().GetObject(gomock.Any(), srcPath, "").
		Return(&s3be.GetObjectResult{Body: io.NopCloser(strings.NewReader("data")), Size: 4}, nil)
	written := new(string)
	dest.EXPECT().PutObject(gomock.Any(), gomock.Any(), gomock.Any(), int64(4), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, key string, _ io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
			*written = key
			return "", errors.New("put failed")
		})
	dest.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, key string) error {
			if key != *written {
				t.Errorf("deleted %q, want the path the copy wrote (%q)", key, *written)
			}
			return nil
		})

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil)
	store.EXPECT().DiscardPending(gomock.Any(), copyIntentOn(destName, 4), RebalanceMove.Reasons.Orphan).
		DoAndReturn(func(_ context.Context, p *core.PendingObject, reason string) ([]core.CleanupItem, error) {
			return []core.CleanupItem{cleanupRow(5, destName, p.StorageKey, reason, 4)}, nil
		})
	store.EXPECT().CompleteCleanupItems(gomock.Any(), retiring(5)).DoAndReturn(retireAll)

	coord := newCoordinatorWith2Backends(sourceName, src, destName, dest, store)
	if _, err := coord.MoveObject(context.Background(), moveReq()); err == nil {
		t.Fatal("expected the copy failure to surface")
	}
}

// TestMoveObject_UnknownBackendClaimsNothing asserts a move naming a backend
// the fleet does not have fails before anything is claimed or copied. The
// store mock has no expectations, so any claim fails the test.
func TestMoveObject_UnknownBackendClaimsNothing(t *testing.T) {
	t.Parallel()
	for _, known := range []string{sourceName, destName} {
		ctrl := gomock.NewController(t)
		coord := newCoordinatorWithBackend(known, backendtest.NewMockObjectBackend(ctrl), NewMockCoordinatorStores(ctrl))
		if _, err := coord.MoveObject(context.Background(), moveReq()); err == nil {
			t.Errorf("move with only %s configured succeeded, want a backend-not-found error", known)
		}
	}
}

// TestMoveObject_RefusedPlacementSettlesTheOrphan verifies a placement the
// store refused still settles the cleanup it queued for the written copy, and
// the error names the sentinel so callers count the move as skipped.
func TestMoveObject_RefusedPlacementSettlesTheOrphan(t *testing.T) {
	t.Parallel()
	for _, declined := range []error{core.ErrCopyChanged, core.ErrTargetRefused} {
		t.Run(declined.Error(), func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			src := backendtest.NewMockObjectBackend(ctrl)
			dest := backendtest.NewMockObjectBackend(ctrl)
			written, _ := expectStreamCopyOK(src, dest)
			dest.EXPECT().DeleteObject(gomock.Any(), gomock.Any()).Return(nil)

			store := NewMockCoordinatorStores(ctrl)
			store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil)
			store.EXPECT().PlaceCopy(gomock.Any(), placementOf(written, RebalanceMove.Reasons)).
				DoAndReturn(func(_ context.Context, p *core.Placement) (int64, []core.CleanupItem, error) {
					return 0, []core.CleanupItem{cleanupRow(9, destName, p.Intent.StorageKey, p.Reasons.Orphan, 4)}, declined
				})
			store.EXPECT().CompleteCleanupItems(gomock.Any(), retiring(9)).DoAndReturn(retireAll)

			coord := newCoordinatorWith2Backends(sourceName, src, destName, dest, store)
			_, err := coord.MoveObject(context.Background(), moveReq())
			if !errors.Is(err, declined) || !core.PlacementDeclined(err) {
				t.Fatalf("err = %v, want a declined placement wrapping %v", err, declined)
			}
		})
	}
}

// TestMoveObject_PlacementErrorIsNotDeclined verifies a placement that failed
// in the store surfaces as a failure rather than a declined placement.
func TestMoveObject_PlacementErrorIsNotDeclined(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src := backendtest.NewMockObjectBackend(ctrl)
	dest := backendtest.NewMockObjectBackend(ctrl)
	written, _ := expectStreamCopyOK(src, dest)

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil)
	store.EXPECT().PlaceCopy(gomock.Any(), placementOf(written, RebalanceMove.Reasons)).Return(int64(0), nil, errDBDown)

	coord := newCoordinatorWith2Backends(sourceName, src, destName, dest, store)
	_, err := coord.MoveObject(context.Background(), moveReq())
	if !errors.Is(err, errDBDown) || core.PlacementDeclined(err) {
		t.Fatalf("err = %v, want the store failure, not a declined placement", err)
	}
}

// reformTo returns a reform that reads the source, records what it read into
// seen, and hands back body as bytes stored in form.
func reformTo(body string, form *core.StoredForm, seen *string) func(context.Context, *s3be.GetObjectResult) (*storedform.Reformed, error) {
	return func(_ context.Context, src *s3be.GetObjectResult) (*storedform.Reformed, error) {
		raw, err := io.ReadAll(src.Body)
		if err != nil {
			return nil, err
		}
		*seen = string(raw)
		return &storedform.Reformed{Body: strings.NewReader(body), Size: int64(len(body)), Form: form}, nil
	}
}

// TestMoveObject_ReformInPlace verifies a move with a reform onto the source's
// own backend claims the reformed size after reading the source and before
// writing, writes the reformed bytes at a new path, and places them as
// reformed.
func TestMoveObject_ReformInPlace(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	be := backendtest.NewMockObjectBackend(ctrl)
	form := &core.StoredForm{CompressionAlgorithm: "zstd", LogicalSize: 4}
	written := new(string)
	get := be.EXPECT().GetObject(gomock.Any(), srcPath, "").
		Return(&s3be.GetObjectResult{Body: io.NopCloser(strings.NewReader("data")), Size: 4}, nil)
	put := be.EXPECT().PutObject(gomock.Any(), gomock.Any(), gomock.Any(), int64(2), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, key string, body io.Reader, _ int64, _ string, _ map[string]string) (string, error) {
			raw, _ := io.ReadAll(body)
			if string(raw) != "zz" {
				t.Errorf("wrote %q, want the reformed bytes", raw)
			}
			*written = key
			return "etag", nil
		})
	be.EXPECT().DeleteObject(gomock.Any(), srcPath).Return(nil)

	store := NewMockCoordinatorStores(ctrl)
	claim := store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Cond(func(p *core.PendingObject) bool {
		return p.BackendName == sourceName && p.SizeBytes == 2 && p.CompressionAlgorithm == "zstd"
	}), core.AcceptForRewrite).Return(true, nil)
	gomock.InOrder(get, claim, put)
	store.EXPECT().PlaceCopy(gomock.Any(), gomock.Cond(func(p *core.Placement) bool {
		return p.Intent.BackendName == sourceName && p.Source.BackendName == sourceName && p.RemoveSource &&
			p.Reformed && p.Intent.StorageKey == *written && *written != srcPath && p.Intent.SizeBytes == 2
	})).Return(int64(2), []core.CleanupItem{cleanupRow(3, sourceName, srcPath, RewriteMove.Reasons.SourceDelete, 4)}, nil)
	store.EXPECT().CompleteCleanupItems(gomock.Any(), retiring(3)).DoAndReturn(retireAll)

	coord := newCoordinatorWithBackend(sourceName, be, store)
	var seen string
	req := moveReq()
	req.DestName = sourceName
	req.Profile = RewriteMove
	req.Reform = reformTo("zz", form, &seen)

	moved, err := coord.MoveObject(context.Background(), req)
	if err != nil {
		t.Fatalf("MoveObject: %v", err)
	}
	if moved != 2 || seen != "data" {
		t.Errorf("moved = %d, reform read %q; want 2 and the source bytes", moved, seen)
	}
}

// TestMoveObject_ReformClaimRefusedWritesNothing verifies a reformed copy the
// destination has no room for is never written, and nothing is discarded
// since nothing was claimed.
func TestMoveObject_ReformClaimRefusedWritesNothing(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src := backendtest.NewMockObjectBackend(ctrl)
	dest := backendtest.NewMockObjectBackend(ctrl)
	src.EXPECT().GetObject(gomock.Any(), srcPath, "").
		Return(&s3be.GetObjectResult{Body: io.NopCloser(strings.NewReader("data")), Size: 4}, nil)

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), copyIntentOn(destName, 7), core.AcceptForRewrite).Return(false, nil)

	coord := newCoordinatorWith2Backends(sourceName, src, destName, dest, store)
	var seen string
	req := moveReq()
	req.Profile = RewriteMove
	req.Reform = reformTo("encoded", &core.StoredForm{}, &seen)
	if _, err := coord.MoveObject(context.Background(), req); !errors.Is(err, core.ErrTargetRefused) {
		t.Fatalf("err = %v, want ErrTargetRefused", err)
	}
}

// TestMoveObject_ReformedCopyFailureDiscards verifies a reformed copy whose
// write fails after its claim is discarded at the reformed size.
func TestMoveObject_ReformedCopyFailureDiscards(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src := backendtest.NewMockObjectBackend(ctrl)
	dest := backendtest.NewMockObjectBackend(ctrl)
	src.EXPECT().GetObject(gomock.Any(), srcPath, "").
		Return(&s3be.GetObjectResult{Body: io.NopCloser(strings.NewReader("data")), Size: 4}, nil)
	dest.EXPECT().PutObject(gomock.Any(), gomock.Any(), gomock.Any(), int64(7), gomock.Any(), gomock.Any()).
		Return("", errors.New("put failed"))

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil)
	store.EXPECT().DiscardPending(gomock.Any(), copyIntentOn(destName, 7), RewriteMove.Reasons.Orphan).Return(nil, nil)

	coord := newCoordinatorWith2Backends(sourceName, src, destName, dest, store)
	var seen string
	req := moveReq()
	req.Profile = RewriteMove
	req.Reform = reformTo("encoded", &core.StoredForm{}, &seen)
	if _, err := coord.MoveObject(context.Background(), req); err == nil {
		t.Fatal("expected the write failure to surface")
	}
}

// TestMoveObject_ReformErrorWritesNothing verifies a failed reform ends the
// move before any claim, destination write or placement.
func TestMoveObject_ReformErrorWritesNothing(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	src := backendtest.NewMockObjectBackend(ctrl)
	dest := backendtest.NewMockObjectBackend(ctrl)
	src.EXPECT().GetObject(gomock.Any(), srcPath, "").
		Return(&s3be.GetObjectResult{Body: io.NopCloser(strings.NewReader("data")), Size: 4}, nil)

	sentinel := errors.New("reform failed")
	coord := newCoordinatorWith2Backends(sourceName, src, destName, dest, NewMockCoordinatorStores(ctrl))
	req := moveReq()
	req.Reform = func(context.Context, *s3be.GetObjectResult) (*storedform.Reformed, error) {
		return nil, sentinel
	}
	_, err := coord.MoveObject(context.Background(), req)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the reform failure", err)
	}
	if ce, ok := errors.AsType[*s3be.CopyError](err); !ok || ce.Phase != s3be.CopyPhaseTransform {
		t.Errorf("err = %v, want a transform-phase copy error", err)
	}
}

// -------------------------------------------------------------------------
// SETTLE, DISCARD, CLAIM, PLACE
// -------------------------------------------------------------------------

// TestSettle_CountsEachRowUnderItsReason verifies every row handed to Settle is
// counted as enqueued under its own reason before its bytes are deleted.
func TestSettle_CountsEachRowUnderItsReason(t *testing.T) {
	t.Parallel()
	const reasonA, reasonB = "settle_count_a", "settle_count_b"
	ctrl := gomock.NewController(t)
	be := backendtest.NewInMemory()
	be.Objects["k!a"] = backendtest.Object{}
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().CompleteCleanupItems(gomock.Any(), retiring(1, 2, 3)).DoAndReturn(retireAll)

	beforeA := promtest.ToFloat64(telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reasonA))
	beforeB := promtest.ToFloat64(telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reasonB))
	coord := newCoordinatorWithBackend("b1", be, store)
	coord.Settle(context.Background(), []core.CleanupItem{
		cleanupRow(1, "b1", "k!a", reasonA, 1),
		cleanupRow(2, "b1", "k!b", reasonA, 1),
		cleanupRow(3, "b1", "k!c", reasonB, 1),
	})

	if got := promtest.ToFloat64(telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reasonA)) - beforeA; got != 2 {
		t.Errorf("enqueued %s = %v, want 2", reasonA, got)
	}
	if got := promtest.ToFloat64(telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(reasonB)) - beforeB; got != 1 {
		t.Errorf("enqueued %s = %v, want 1", reasonB, got)
	}
	if be.Has("k!a") {
		t.Error("settled bytes are still on the backend")
	}
}

// TestSettle_NothingToSettle verifies an empty batch touches neither the store
// nor any backend.
func TestSettle_NothingToSettle(t *testing.T) {
	t.Parallel()
	coord := newCoordinatorWithStore(NewMockCoordinatorStores(gomock.NewController(t)))
	coord.Settle(context.Background(), nil)
}

// TestDiscard_SettlesWhatTheDiscardQueued verifies a discarded intent's bytes
// are deleted at the intent's own path and the row is retired.
func TestDiscard_SettlesWhatTheDiscardQueued(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	be := backendtest.NewInMemory()
	p := NewCopyIntent("k", "b1", 4, nil)
	be.Objects[p.StorageKey] = backendtest.Object{}
	be.Objects["k!other"] = backendtest.Object{}

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().DiscardPending(gomock.Any(), p, "discard_reason").
		Return([]core.CleanupItem{cleanupRow(4, "b1", p.StorageKey, "discard_reason", 4)}, nil)
	store.EXPECT().CompleteCleanupItems(gomock.Any(), retiring(4)).DoAndReturn(retireAll)

	coord := newCoordinatorWithBackend("b1", be, store)
	coord.Discard(context.Background(), p, "discard_reason")

	if be.Has(p.StorageKey) {
		t.Error("the discarded intent's bytes are still on the backend")
	}
	if !be.Has("k!other") {
		t.Error("the discard reached bytes that were not the intent's")
	}
}

// TestDiscard_ErrorLeavesTheBytes verifies a failed discard deletes nothing, so
// the intent and its bytes stay for the reaper.
func TestDiscard_ErrorLeavesTheBytes(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	be := backendtest.NewInMemory()
	p := NewCopyIntent("k", "b1", 4, nil)
	be.Objects[p.StorageKey] = backendtest.Object{}

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().DiscardPending(gomock.Any(), p, "discard_reason").Return(nil, errDBDown)

	coord := newCoordinatorWithBackend("b1", be, store)
	coord.Discard(context.Background(), p, "discard_reason")

	if !be.Has(p.StorageKey) {
		t.Error("a failed discard deleted the intent's bytes")
	}
}

// TestNewCopyIntent_IsACompanionOnTheTarget verifies a copy intent names its
// target, is never promoted, and carries a path of its own.
func TestNewCopyIntent_IsACompanionOnTheTarget(t *testing.T) {
	t.Parallel()
	form := &core.StoredForm{ContentHash: "abc"}
	p := NewCopyIntent("k", "b1", 4, form)
	if p.BackendName != "b1" || p.Role != core.PendingRoleCompanion || p.SizeBytes != 4 {
		t.Errorf("intent = %+v, want a 4-byte companion on b1", p)
	}
	if p.ContentHash != "abc" {
		t.Errorf("ContentHash = %q, want the form's", p.ContentHash)
	}
	if p.StorageKey == "" || p.StorageKey == "k" {
		t.Errorf("StorageKey = %q, want a path minted for the intent", p.StorageKey)
	}
}

// TestClaimCopyTarget covers the three outcomes of claiming a copy target: an
// accepted claim, a backend that declines, and a database error.
func TestClaimCopyTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		fits    bool
		err     error
		wantErr error
	}{
		{"accepted", true, nil, nil},
		{"declined", false, nil, core.ErrTargetRefused},
		{"store error", false, errDBDown, errDBDown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			store := NewMockCoordinatorStores(ctrl)
			p := NewCopyIntent("k", "b1", 4, nil)
			store.EXPECT().InsertPendingIfFits(gomock.Any(), p, core.AcceptForPart).Return(tc.fits, tc.err)

			coord := newCoordinatorWithStore(store)
			err := coord.ClaimCopyTarget(context.Background(), p, core.AcceptForPart)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.err != nil && errors.Is(err, core.ErrTargetRefused) {
				t.Error("a database error was reported as a refused target")
			}
		})
	}
}

// TestPlace_SettlesAndReturnsWhatPlaceCopyReturned verifies Place settles the
// cleanup the placement queued whether or not the placement committed.
func TestPlace_SettlesAndReturnsWhatPlaceCopyReturned(t *testing.T) {
	t.Parallel()
	for _, placeErr := range []error{nil, core.ErrTargetRefused} {
		name := "committed"
		if placeErr != nil {
			name = placeErr.Error()
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			be := backendtest.NewInMemory()
			be.Objects["k!queued"] = backendtest.Object{}
			store := NewMockCoordinatorStores(ctrl)
			p := &core.Placement{Intent: NewCopyIntent("k", "b1", 4, nil), Reasons: ReplicationReasons}
			store.EXPECT().PlaceCopy(gomock.Any(), p).
				Return(int64(9), []core.CleanupItem{cleanupRow(2, "b1", "k!queued", ReplicationReasons.Orphan, 4)}, placeErr)
			store.EXPECT().CompleteCleanupItems(gomock.Any(), retiring(2)).DoAndReturn(retireAll)

			coord := newCoordinatorWithBackend("b1", be, store)
			placed, err := coord.Place(context.Background(), p)
			if !errors.Is(err, placeErr) || placed != 9 {
				t.Fatalf("Place = (%d, %v), want (9, %v)", placed, err, placeErr)
			}
			if be.Has("k!queued") {
				t.Error("the queued bytes were not settled")
			}
		})
	}
}

// -------------------------------------------------------------------------
// INTENTS AND WRITE TARGETS
// -------------------------------------------------------------------------

// TestNewPendingIntent_CopiesStoredForm drives the form != nil branch so the
// PendingObject carries the wrapped DEK, keyID, plaintext size, and content
// hash. The intent is the only record of how the bytes were written until the
// commit lands, so a field missing here is a crash-recovered object whose row
// describes something nothing can read.
func TestNewPendingIntent_CopiesStoredForm(t *testing.T) {
	t.Parallel()
	form := &core.StoredForm{
		Encrypted:     true,
		EncryptionKey: []byte("wrapped-dek-bytes"),
		KeyID:         "kid-1",
		PlaintextSize: 4096,
		ContentHash:   "deadbeef",
	}

	got := NewPendingIntent("k", 4096, form, nil)

	if got.IntentID == "" {
		t.Fatal("expected a minted intent ID")
	}
	if got.BackendName != "" {
		t.Errorf("backend = %q, want it left for the claim to decide", got.BackendName)
	}
	if !got.Encrypted || got.KeyID != "kid-1" || got.PlaintextSize != 4096 || got.ContentHash != "deadbeef" {
		t.Errorf("encryption metadata not copied onto PendingObject: %+v", got)
	}
	if string(got.EncryptionKey) != "wrapped-dek-bytes" {
		t.Errorf("EncryptionKey not copied: %q", got.EncryptionKey)
	}
}

// TestNewPendingIntent_CopiesCompression asserts an intent carries the stored
// form's compression fields, and that SizeBytes is the stored size quota is
// judged against, not the larger logical size.
func TestNewPendingIntent_CopiesCompression(t *testing.T) {
	t.Parallel()
	form := &core.StoredForm{
		ContentHash:              "deadbeef",
		CompressionAlgorithm:     "zstd",
		CompressionLevel:         "default",
		CompressionFormatVersion: 1,
		LogicalSize:              8192,
	}

	got := NewPendingIntent("k", 4096, form, nil)

	if got.CompressionAlgorithm != "zstd" {
		t.Errorf("CompressionAlgorithm = %q, want %q", got.CompressionAlgorithm, "zstd")
	}
	if got.CompressionLevel != "default" {
		t.Errorf("CompressionLevel = %q, want %q", got.CompressionLevel, "default")
	}
	if got.CompressionFormatVersion != 1 {
		t.Errorf("CompressionFormatVersion = %d, want 1", got.CompressionFormatVersion)
	}
	if got.LogicalSize != 8192 {
		t.Errorf("LogicalSize = %d, want 8192", got.LogicalSize)
	}
	if got.SizeBytes != 4096 {
		t.Errorf("SizeBytes = %d, want the 4096 landing on the backend", got.SizeBytes)
	}
}

// TestClaimWriteTarget_TriesTheNextCandidateWhenOneIsFull asserts a backend
// whose insert declines is skipped rather than fatal. Declining is how a full
// backend reports itself now, so it has to read as "try the next one" and not
// as an error.
func TestClaimWriteTarget_TriesTheNextCandidateWhenOneIsFull(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)

	var claimed []string
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), core.AcceptForWrite).
		DoAndReturn(func(_ context.Context, p *core.PendingObject, _ core.BackendFlags) (bool, error) {
			claimed = append(claimed, p.BackendName)
			return p.BackendName == "b2", nil
		}).Times(2)

	coord := newCoordinatorWithStore(store)

	name, err := coord.ClaimWriteTarget(context.Background(),
		NewPendingIntent("k", 4096, nil, nil), []string{"b1", "b2"})
	if err != nil {
		t.Fatalf("ClaimWriteTarget: %v", err)
	}
	if name != "b2" {
		t.Errorf("claimed %q, want b2 after b1 declined", name)
	}
	if want := []string{"b1", "b2"}; !slices.Equal(claimed, want) {
		t.Errorf("tried %v, want %v in order", claimed, want)
	}
}

// TestClaimWriteTarget_NoCandidateFits asserts the caller is told there is no
// room rather than being handed a backend that refused, so the request ends as
// insufficient storage instead of a write against a full backend.
func TestClaimWriteTarget_NoCandidateFits(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(false, nil).Times(2)

	coord := newCoordinatorWithStore(store)

	if _, err := coord.ClaimWriteTarget(context.Background(),
		NewPendingIntent("k", 4096, nil, nil), []string{"b1", "b2"}); !errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Errorf("err = %v, want ErrNoSpaceAvailable", err)
	}
}

// TestClaimWriteTarget_StoreError covers the insert failure branch: a database
// error is surfaced rather than read as a full backend, because treating an
// outage as "no room" would silently place the write elsewhere.
func TestClaimWriteTarget_StoreError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(false, errDBDown).Times(1)

	coord := newCoordinatorWithStore(store)

	name, err := coord.ClaimWriteTarget(context.Background(),
		NewPendingIntent("k", 4096, nil, nil), []string{"b1", "b2"})
	if err == nil {
		t.Fatal("expected the insert failure to surface")
	}
	if errors.Is(err, core.ErrNoSpaceAvailable) {
		t.Error("a database error was reported as a full backend")
	}
	if name != "" {
		t.Errorf("claimed %q, want no backend on error", name)
	}
}

// -------------------------------------------------------------------------
// RECORD
// -------------------------------------------------------------------------

// recordReq is the single-copy record request the record tests share.
func recordReq() *core.RecordObjectRequest {
	return &core.RecordObjectRequest{
		Key: "k", Size: 100,
		Copies: []core.ObjectCopy{{Backend: "new", IntentID: "i-1", StorageKey: "k!new"}},
	}
}

// recordAuditEvents captures every audit event emitted until the test ends.
// Settle emits from several goroutines, so reads go through the returned func.
func recordAuditEvents(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var events []string
	audit.SetOnEvent(func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	})
	t.Cleanup(func() { audit.SetOnEvent(nil) })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(events)
	}
}

// TestRecordObjectAndPromoteIntent_SettlesWhatTheCommitDisplaced verifies the
// rows a commit queued are deleted at their own paths and retired, and the
// overwrite is audited. Each row names where its bytes are, which is not the
// object's key and not what this write just stored.
func TestRecordObjectAndPromoteIntent_SettlesWhatTheCommitDisplaced(t *testing.T) {
	ctrl := gomock.NewController(t)
	old := backendtest.NewMockObjectBackend(ctrl)
	stale := backendtest.NewMockObjectBackend(ctrl)
	old.EXPECT().DeleteObject(gomock.Any(), "k!replaced").Return(nil)
	stale.EXPECT().DeleteObject(gomock.Any(), "k!superseded").Return(nil)

	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).Return([]core.CleanupItem{
		cleanupRow(1, "old", "k!replaced", core.CleanupReasonOverwritten, 50),
		cleanupRow(2, "stale", "k!superseded", core.CleanupReasonSupersededIntent, 70),
	}, nil)
	store.EXPECT().CompleteCleanupItems(gomock.Any(), retiring(1, 2)).DoAndReturn(retireAll)
	events := recordAuditEvents(t)

	coord := newCoordinatorWith2Backends("old", old, "stale", stale, store)
	if err := coord.RecordObjectAndPromoteIntent(context.Background(), testSpan(t), recordReq()); err != nil {
		t.Fatalf("RecordObjectAndPromoteIntent: %v", err)
	}
	if events := events(); !slices.Contains(events, "storage.overwrite_displaced") {
		t.Errorf("audit events = %v, want storage.overwrite_displaced", events)
	}
}

// TestRecordObjectAndPromoteIntent_FreshKeyAuditsNothing verifies a commit that
// displaced nothing settles nothing and records no overwrite.
func TestRecordObjectAndPromoteIntent_FreshKeyAuditsNothing(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).Return(nil, nil)
	events := recordAuditEvents(t)

	coord := newCoordinatorWithStore(store)
	if err := coord.RecordObjectAndPromoteIntent(context.Background(), testSpan(t), recordReq()); err != nil {
		t.Fatalf("RecordObjectAndPromoteIntent: %v", err)
	}
	if slices.Contains(events(), "storage.overwrite_displaced") {
		t.Error("a commit that displaced nothing was audited as an overwrite")
	}
}

// TestRankReplicaTargets_ExcludesExistingCopies asserts a backend that already
// holds a copy is never proposed as a replica target.
func TestRankReplicaTargets_ExcludesExistingCopies(t *testing.T) {
	t.Parallel()
	names := []string{"b1", "b2", "b3"}
	coord := New(infra.New(&infra.Config{
		Backends: map[string]s3be.ObjectBackend{"b1": backendtest.NewInMemory(), "b2": backendtest.NewInMemory(), "b3": backendtest.NewInMemory()},
		Order:    names,
		Usage:    counter.NewUsageTracker(counter.NewLocalCounterBackend(names), nil),
		Quota:    counter.NewQuotaTracker(),
	}), NewMockCoordinatorStores(gomock.NewController(t)))

	if got := coord.RankReplicaTargets(50, map[string]bool{"b1": true, "b2": true}); !slices.Equal(got, []string{"b3"}) {
		t.Errorf("RankReplicaTargets = %v, want [b3]", got)
	}
}

// TestRecordObjectAndPromoteIntent_StoreErrorChargesThePut verifies a failed
// commit surfaces, still charges the PUT the backend served, and deletes
// nothing: the intent stays for the reaper.
func TestRecordObjectAndPromoteIntent_StoreErrorChargesThePut(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().RecordObject(gomock.Any(), gomock.Any()).Return(nil, errDBDown)

	counters := counter.NewLocalCounterBackend([]string{"new"})
	coord := New(infra.New(&infra.Config{
		Backends: map[string]s3be.ObjectBackend{"new": backendtest.NewInMemory()},
		Usage:    counter.NewUsageTracker(counters, nil),
		Quota:    counter.NewQuotaTracker(),
	}), store)
	err := coord.RecordObjectAndPromoteIntent(context.Background(), testSpan(t), recordReq())
	if !errors.Is(err, errDBDown) {
		t.Fatalf("err = %v, want the store failure", err)
	}
	if got := loadUsage(counters, "new").APIRequests; got != 1 {
		t.Errorf("api calls = %d, want the one PUT charged", got)
	}
}

// TestRecordObjectAndPromoteIntent_RejectsMultipleCopies verifies the helper
// refuses a request placing more than one copy, since it accounts for one
// upload only.
func TestRecordObjectAndPromoteIntent_RejectsMultipleCopies(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)

	coord := newCoordinatorWithStore(store)

	tracer := noop.NewTracerProvider().Tracer("test")
	_, sp := tracer.Start(context.Background(), "test")
	defer sp.End()

	err := coord.RecordObjectAndPromoteIntent(context.Background(), sp, &core.RecordObjectRequest{
		Key: "k", Size: 100,
		Copies: []core.ObjectCopy{{Backend: "b1", IntentID: "i-1"}, {Backend: "b2", IntentID: "i-2"}},
	})
	if err == nil {
		t.Fatal("expected a multi-copy request to be refused")
	}
}

// testSpan returns a no-op span for calls that take one.
func testSpan(t *testing.T) trace.Span {
	t.Helper()
	_, sp := noop.NewTracerProvider().Tracer("test").Start(context.Background(), "test")
	t.Cleanup(func() { sp.End() })
	return sp
}

// -------------------------------------------------------------------------
// UPLOAD TARGETS
// -------------------------------------------------------------------------

// TestClaimUploadTarget_SkipsADecliningBackend verifies an upload the first
// candidate's insert declines, as a draining backend does, is recorded on the
// next candidate instead.
func TestClaimUploadTarget_SkipsADecliningBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	var tried []string
	store.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.CreateMultipartUploadParams) (bool, error) {
			tried = append(tried, p.BackendName)
			return len(tried) == 2, nil
		}).Times(2)

	coord := newCoordinatorWith2Backends("a", backendtest.NewMockObjectBackend(ctrl), "b", backendtest.NewMockObjectBackend(ctrl), store)
	params := &core.CreateMultipartUploadParams{UploadID: "u-1", ObjectKey: "k"}
	name, err := coord.ClaimUploadTarget(context.Background(), testSpan(t), s3op.CreateMultipartUpload, params)
	if err != nil {
		t.Fatalf("ClaimUploadTarget: %v", err)
	}
	if name != "b" || params.BackendName != "b" {
		t.Errorf("claimed %q (params %q), want b after a declined", name, params.BackendName)
	}
	if want := []string{"a", "b"}; !slices.Equal(tried, want) {
		t.Errorf("tried %v, want %v in order", tried, want)
	}
}

// TestClaimUploadTarget_EveryBackendDeclines verifies the claim reports no space
// when no candidate accepts the upload.
func TestClaimUploadTarget_EveryBackendDeclines(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).Return(false, nil).Times(2)

	coord := newCoordinatorWith2Backends("a", backendtest.NewMockObjectBackend(ctrl), "b", backendtest.NewMockObjectBackend(ctrl), store)
	_, err := coord.ClaimUploadTarget(context.Background(), testSpan(t), s3op.CreateMultipartUpload,
		&core.CreateMultipartUploadParams{UploadID: "u-1", ObjectKey: "k"})
	if err == nil {
		t.Fatal("claimed an upload target when every backend declined")
	}
}

// TestClaimUploadTarget_DatabaseErrorEndsTheWalk verifies a store error is
// returned at once rather than read as a decline and tried elsewhere.
func TestClaimUploadTarget_DatabaseErrorEndsTheWalk(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := NewMockCoordinatorStores(ctrl)
	store.EXPECT().CreateMultipartUpload(gomock.Any(), gomock.Any()).Return(false, errDBDown).Times(1)

	coord := newCoordinatorWith2Backends("a", backendtest.NewMockObjectBackend(ctrl), "b", backendtest.NewMockObjectBackend(ctrl), store)
	if _, err := coord.ClaimUploadTarget(context.Background(), testSpan(t), s3op.CreateMultipartUpload,
		&core.CreateMultipartUploadParams{UploadID: "u-1", ObjectKey: "k"}); err == nil {
		t.Fatal("ClaimUploadTarget returned no error for a database failure")
	}
}
