// -------------------------------------------------------------------------------
// Write Coordinator - Shared Write-Path Helpers
//
// Author: Alex Freidah
//
// Owns the helpers that combine the per-role store views with the backend
// runtime primitives to record objects, promote pending intents, settle the
// cleanups their transactions queue, and pick write targets. The object and multipart managers hold a
// *Coordinator directly, so each is fully initialised at construction time
// without post-construction patching.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"go.opentelemetry.io/otel/trace"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/cleanup"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// -------------------------------------------------------------------------
// TYPE
// -------------------------------------------------------------------------

// CoordinatorStores is the narrow persistence surface the write coordinator needs:
// object record/move, write-target selection (quota), pending-intent
// insert/promote/discard, and settling queued cleanups. Declared locally so
// writepath does not pull in the full MetadataStore.
//
//go:generate mockgen -destination=mock_stores_test.go -package=writepath github.com/afreidah/s3-orchestrator/internal/proxy/writepath CoordinatorStores
type CoordinatorStores interface {
	core.ObjectStore
	core.QuotaStore
	core.PendingStore
	core.CleanupStore
	CreateMultipartUpload(ctx context.Context, params *core.CreateMultipartUploadParams) (bool, error)
}

// Coordinator bundles the infrastructure subset (WriteRuntime) with the
// metadata-store contract so the write-path helpers can be expressed as plain
// methods on one value every consumer shares, with no post-construction wiring
// step.
type Coordinator struct {
	core    WriteRuntime // infrastructure subset: backends, usage, routing, eligibility, error classification, deletes
	stores  CoordinatorStores
	settler *cleanup.Settler
	log     *slog.Logger
}

// settleConcurrency bounds how many backends a request deletes from at once,
// and how many of its cleanup rows it settles at once.
const settleConcurrency = 8

// New constructs a Coordinator. The supplied core must observe the same
// admission, usage, drain, and backend state every other collaborator sees;
// in production they are all handed the one *infra.BackendRuntime. The
// component-scoped logger is built in the constructor body per the
// project's logging convention.
func New(core WriteRuntime, stores CoordinatorStores) *Coordinator {
	must.NotNil("core", core)
	must.NotNil("stores", stores)
	return &Coordinator{
		core:    core,
		stores:  stores,
		settler: cleanup.New(cleanup.Deps{Deleter: core, Store: stores, Concurrency: settleConcurrency}),
		log:     slog.Default().With(logfmt.Component("writepath")),
	}
}

// -------------------------------------------------------------------------
// ROUTING
// -------------------------------------------------------------------------

// ClaimWriteTarget picks the target backend for a write using the configured
// routing strategy and claims the bytes on it with the write's intent. "pack"
// takes the first eligible backend with room, "spread" the least utilized one.
// Returns ErrNoSpaceAvailable when no candidate has room.
func (w *Coordinator) ClaimWriteTarget(ctx context.Context, p *core.PendingObject, eligible []string) (string, error) {
	name, err := w.claimFirst(eligible, func(name string) (bool, error) {
		p.BackendName = name
		return w.stores.InsertPendingIfFits(ctx, p, core.AcceptForWrite)
	})
	if err != nil {
		return "", err
	}
	w.noteClaim(p)
	return name, nil
}

// noteClaim tells the ranking what this instance just placed, or every write
// in the interval before the next reload ranks the candidates identically and
// spread stops spreading.
func (w *Coordinator) noteClaim(p *core.PendingObject) {
	w.core.Quota().NotePlacement(p.BackendName, p.SizeBytes)
	telemetry.PendingIntentsEnqueuedTotal.Inc()
}

// claimFirst walks the eligible backends in routing order and returns the first
// whose conditional insert accepts.
//
// The order comes from a possibly stale snapshot; room is decided by the insert
// against live rows. A declined candidate is skipped. Returns
// ErrNoSpaceAvailable when none accepts, and stops on a database error.
func (w *Coordinator) claimFirst(eligible []string, try func(name string) (bool, error)) (string, error) {
	for _, name := range w.rankForWrite(w.core.Quota(), eligible) {
		ok, err := try(name)
		if err != nil {
			return "", fmt.Errorf("claim write target: %w", err)
		}
		if ok {
			return name, nil
		}
		telemetry.QuotaClaimsDeclinedTotal.WithLabelValues(name).Inc()
	}
	return "", core.ErrNoSpaceAvailable
}

// ClaimWriteCopies claims a distinct backend for each intent in one
// transaction, using the same conditional insert a single-copy write uses, and
// returns the intents that got one, in claim order, with BackendName set.
// Claiming fewer than asked is not an error, since the replicator makes up the
// rest; only claiming none fails.
func (w *Coordinator) ClaimWriteCopies(ctx context.Context, intents []*core.PendingObject, eligible []string) ([]*core.PendingObject, error) {
	claims, err := w.stores.ClaimPendingCopies(ctx, intents, w.rankForWrite(w.core.Quota(), eligible), core.AcceptForWrite)
	if err != nil {
		return nil, fmt.Errorf("claim write targets: %w", err)
	}
	for _, name := range claims.Declined {
		telemetry.QuotaClaimsDeclinedTotal.WithLabelValues(name).Inc()
	}
	for _, p := range claims.Claimed {
		w.noteClaim(p)
	}
	if len(claims.Claimed) == 0 {
		return nil, core.ErrNoSpaceAvailable
	}
	return claims.Claimed, nil
}

// rankForWrite orders the candidates the way the configured strategy wants them
// tried: pack keeps the configured order so writes fill one backend before
// moving on, spread puts the least utilized first. The slice is copied before
// sorting so the caller's eligibility list is left alone.
func (w *Coordinator) rankForWrite(quota *counter.QuotaTracker, eligible []string) []string {
	if w.core.RoutingStrategy() != config.RoutingSpread {
		return eligible
	}
	return quota.RankByUtilization(eligible)
}

// SelectWriteTarget picks a backend for a write and claims it with the pending
// intent p. Returns ErrInsufficientStorage when no backend can accept the
// write, or the classified selection error. The caller owns the intent from
// here: the transaction that records the object clears it, and any path that
// gives up leaves it for the reaper.
func (w *Coordinator) SelectWriteTarget(ctx context.Context, span trace.Span, operation s3op.Operation, p *core.PendingObject) (string, error) {
	eligible := w.core.EligibleForWrite([]s3op.Operation{operation}, 0, p.SizeBytes)
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation.String(), "write").Inc()
		observe.MarkSpanError(span, "usage limits exceeded on all backends")
		return "", core.ErrInsufficientStorage
	}
	name, err := w.ClaimWriteTarget(ctx, p, eligible)
	if err != nil {
		return "", w.core.ClassifyWriteError(span, operation.String(), err)
	}
	return name, nil
}

// -------------------------------------------------------------------------
// RECORD + CLEANUP
// -------------------------------------------------------------------------

// soleBackend names the single copy a request places, or reports why it cannot.
// RecordObjectAndPromoteIntent accounts for one upload, so a request naming any
// other number is a caller error rather than a state it can recover from.
func soleBackend(req *core.RecordObjectRequest) (string, error) {
	if len(req.Copies) != 1 {
		return "", fmt.Errorf("%w: record request for %s names %d copies", errSingleCopyOnly, req.Key, len(req.Copies))
	}
	return req.Copies[0].Backend, nil
}

// errSingleCopyOnly reports a multi-copy request handed to a helper whose
// recovery path can only account for one.
var errSingleCopyOnly = errors.New("write path: helper records a single copy")

// Settle deletes the bytes behind cleanup rows a transaction just queued and
// settles each row: retired once the bytes are gone, retried with backoff when
// the delete fails. The rows already name the bytes durably, so a failure here
// leaves nothing untracked. Deletes are not gated on usage limits: refusing one
// over budget would leave an operator unable to get back under it.
func (w *Coordinator) Settle(ctx context.Context, items []core.CleanupItem) {
	for i := range items {
		telemetry.CleanupQueueEnqueuedTotal.WithLabelValues(items[i].Reason).Inc()
	}
	w.settler.Settle(ctx, items)
}

// Discard drops an intent whose upload will not be recorded and settles the
// cleanup of its bytes. When the discard itself fails the intent stays, and the
// pending reaper resolves it on a later tick.
func (w *Coordinator) Discard(ctx context.Context, p *core.PendingObject, reason string) {
	cleanup, err := w.stores.DiscardPending(ctx, p, reason)
	if err != nil {
		w.log.ErrorContext(ctx, "discard of an unrecorded upload failed; intent left for reaper",
			"key", p.ObjectKey, "backend", p.BackendName, "intent_id", p.IntentID, logfmt.Err(err))
		return
	}
	w.Settle(ctx, cleanup)
}

// NewPendingIntent builds the intent a write is admitted on. ClaimWriteTarget
// fills in the backend, since the same insert checks room. Admission subtracts
// held intents from headroom, so in-progress bytes count for every instance.
//
// size is what lands on the backend and id is the identity a reaper-promoted
// object answers HEAD with. The intent id also names the copy's own storage
// path, so cleaning it up can only remove this intent's bytes.
func NewPendingIntent(key string, size int64, form *core.StoredForm, id *core.ObjectIdentity) *core.PendingObject {
	p := &core.PendingObject{
		IntentID:  audit.NewID(),
		ObjectKey: key,
		SizeBytes: size,
		Identity:  id,
	}
	p.StorageKey = internalkey.StorageKey(key, p.IntentID)
	if form != nil {
		p.StoredForm = *form
	}
	return p
}

// NewCopyIntent builds the intent a replica, move or rewrite claims its target
// with before copying. It is a companion: the reaper never promotes it, and
// discards its bytes unless a recorded copy already sits at its path. size is
// what the copy will land on the backend and form how those bytes are stored.
func NewCopyIntent(key, target string, size int64, form *core.StoredForm) *core.PendingObject {
	p := NewPendingIntent(key, size, form, nil)
	p.BackendName = target
	p.Role = core.PendingRoleCompanion
	return p
}

// ClaimCopyTarget claims the intent's backend for a copy, admitting the bytes
// on a backend whose flags all fall within accepts. Returns core.ErrTargetRefused
// when the backend has no room or refuses the copy, so nothing was written.
func (w *Coordinator) ClaimCopyTarget(ctx context.Context, p *core.PendingObject, accepts core.BackendFlags) error {
	fits, err := w.stores.InsertPendingIfFits(ctx, p, accepts)
	if err != nil {
		return fmt.Errorf("claim copy target %s: %w", p.BackendName, err)
	}
	if !fits {
		telemetry.QuotaClaimsDeclinedTotal.WithLabelValues(p.BackendName).Inc()
		return core.ErrTargetRefused
	}
	w.noteClaim(p)
	return nil
}

// Place records a copy at the path its intent claimed and settles the cleanup
// the placement queued: the source of a move, or the copy itself when the
// placement was refused. Returns what PlaceCopy returned.
func (w *Coordinator) Place(ctx context.Context, p *core.Placement) (int64, error) {
	placed, cleanup, err := w.stores.PlaceCopy(ctx, p)
	w.Settle(ctx, cleanup)
	return placed, err
}

// RecordObjectAndPromoteIntent commits the object location, updates quota, and
// clears the pending intent in one transaction. On failure the intent and the
// backend bytes are left for the pending reaper, which HEADs the backend and
// either promotes or removes the intent.
func (w *Coordinator) RecordObjectAndPromoteIntent(ctx context.Context, span trace.Span, req *core.RecordObjectRequest) error {
	backendName, err := soleBackend(req)
	if err != nil {
		observe.RecordSpanError(span, err)
		return err
	}
	intentID := req.Copies[0].IntentID

	cleanup, _, err := w.stores.RecordObject(ctx, req)
	if err == nil {
		telemetry.PendingIntentsResolvedTotal.WithLabelValues("committed").Inc()
	}
	if err != nil {
		// The intent stays, so the bytes stay claimed against the backend
		// until whichever pass resolves it - the reaper's promotion or its
		// removal - settles what they are worth.
		w.log.ErrorContext(ctx, "recordObject failed; intent left for reaper",
			"key", req.Key, "backend", backendName, "intent_id", intentID, "error", err)
		// The successful PUT against the backend still consumed an API
		// call. The success-path usage record runs only when this returns
		// nil, so account for it here.
		w.core.Acct().APICall(ctx, s3op.PutObject, backendName)
		observe.RecordSpanError(span, err)
		return fmt.Errorf("failed to record object: %w", err)
	}
	w.settleDisplaced(ctx, req.Key, backendName, cleanup)
	return nil
}

// CommitCompanionCopy records an extra copy whose upload finished after the
// client was answered, and cleans up after it when a newer write took the key
// first. The copy is added to the key rather than replacing what it holds, so
// the copy that answered the client stays. It reports whether the copy was
// recorded; a copy discarded because a newer write took the key is not an
// error.
func (w *Coordinator) CommitCompanionCopy(ctx context.Context, p *core.PendingObject) (recorded bool, err error) {
	result, cleanup, _, err := w.stores.CommitCompanionCopy(ctx, p)
	if err != nil {
		// The intent stays, so the reaper resolves the copy on a later tick -
		// discarding its bytes, since an extra copy is never promoted.
		w.log.ErrorContext(ctx, "commit of a further copy failed; intent left for reaper",
			"key", p.ObjectKey, "backend", p.BackendName, "intent_id", p.IntentID, logfmt.Err(err))
		w.core.Acct().APICall(ctx, s3op.PutObject, p.BackendName)
		telemetry.ReplicationWriteCopiesTotal.WithLabelValues(WriteCopyFailed).Inc()
		return false, err
	}
	if result == core.CompanionCopyCommitted {
		w.core.Acct().Ingress(ctx, s3op.PutObject, p.BackendName, p.SizeBytes)
		telemetry.ReplicationWriteCopiesTotal.WithLabelValues(WriteCopyCommitted).Inc()
		return true, nil
	}
	w.log.WarnContext(ctx, "a newer write took the key while a further copy was uploading; discarding it",
		"key", p.ObjectKey, "backend", p.BackendName, "intent_id", p.IntentID)
	w.core.Acct().APICall(ctx, s3op.PutObject, p.BackendName)
	w.Settle(ctx, cleanup)
	telemetry.ReplicationWriteCopiesTotal.WithLabelValues(WriteCopyUntrusted).Inc()
	return false, nil
}

// The outcomes ReplicationWriteCopiesTotal counts, shared with the write path
// so the label set is written once.
const (
	WriteCopyCommitted = "committed"
	WriteCopyUntrusted = "untrusted"
	WriteCopyFailed    = "failed"
)

// settleDisplaced settles the cleanup an overwrite queued for the copies it
// displaced, and audits the overwrite.
func (w *Coordinator) settleDisplaced(ctx context.Context, key, newBackend string, displaced []core.CleanupItem) {
	w.Settle(ctx, displaced)
	if len(displaced) > 0 {
		audit.Log(ctx, "storage.overwrite_displaced",
			slog.String("key", key),
			slog.String("new_backend", newBackend),
			slog.Int("displaced_copies", len(displaced)),
		)
	}
}

// -------------------------------------------------------------------------
// SHARED OBJECT MOVE PRIMITIVE
// -------------------------------------------------------------------------

// MoveRequest bundles the inputs to a single src -> dest object move. Source
// is the copy the caller read; the move commits only while it is still the
// copy recorded on its backend. The destination is claimed with an intent at a
// path minted for the move, so each cleanup deletes exactly its own bytes.
//
// Reform, when set, rewrites the bytes on the way and reports the form they
// then carry. A move with a reform may name the source's own backend as its
// destination, which is how a rewrite pass replaces a copy.
type MoveRequest struct {
	Source      *core.ObjectLocation
	SrcBackend  backend.ObjectBackend
	DestBackend backend.ObjectBackend
	DestName    string
	Profile     MoveProfile
	Reform      func(ctx context.Context, src *backend.GetObjectResult) (*storedform.Reformed, error)
}

// MoveProfile is how one subsystem moves objects: the labels it queues bytes
// under, and the backend flags its destination may carry.
type MoveProfile struct {
	Reasons core.MoveReasons
	Accepts core.BackendFlags
}

// RebalanceMove, DrainMove and RewriteMove are the profiles of the subsystems
// that move objects. A rewrite replaces a copy on its own backend, so a
// cordoned backend still takes it. ReplicationReasons labels a replica that
// was never recorded.
var (
	RebalanceMove = MoveProfile{
		Reasons: core.MoveReasons{Orphan: "rebalance_orphan", SourceDelete: "rebalance_source_delete"},
		Accepts: core.AcceptForWrite,
	}
	DrainMove = MoveProfile{
		Reasons: core.MoveReasons{Orphan: "drain_orphan", SourceDelete: "drain_source_delete"},
		Accepts: core.AcceptForWrite,
	}
	RewriteMove = MoveProfile{
		Reasons: core.MoveReasons{Orphan: "rewrite_orphan", SourceDelete: "rewrite_source_delete"},
		Accepts: core.AcceptForRewrite,
	}
	ReplicationReasons = core.MoveReasons{Orphan: "replication_orphan"}
)

// MoveObject moves one object from src to dest for drain, rebalance and the
// rewrite passes: it claims dest for the bytes, copies them, reformed when the
// request says how, and places them while removing the source through
// PlaceCopy, which queues the source's bytes for the cleanup settled here.
// Returns the moved size, or the wrapped failure: core.ErrCopyChanged when a
// newer write replaced the source, core.ErrTargetRefused when dest holds a
// copy or has no room for the bytes.
func (w *Coordinator) MoveObject(ctx context.Context, req *MoveRequest) (int64, error) {
	key, srcName := req.Source.ObjectKey, req.Source.BackendName
	intent := NewCopyIntent(key, req.DestName, req.Source.SizeBytes, &req.Source.StoredForm)
	copyReq := &backend.CopyRequest{
		Src:          backend.CopyEndpoint{Name: srcName, Backend: req.SrcBackend},
		Dst:          backend.CopyEndpoint{Name: req.DestName, Backend: req.DestBackend},
		SrcKey:       core.StoragePath(key, req.Source.StorageKey),
		DstKey:       intent.StorageKey,
		SizeEstimate: req.Source.SizeBytes,
	}
	placement := &core.Placement{Source: req.Source, Intent: intent, RemoveSource: true, Reasons: req.Profile.Reasons}
	claimed := false
	if req.Reform == nil {
		if err := w.ClaimCopyTarget(ctx, intent, req.Profile.Accepts); err != nil {
			return 0, fmt.Errorf("claim %s for %s: %w", req.DestName, key, err)
		}
		claimed = true
	} else {
		// A reform learns its size only once it has encoded the bytes, so the
		// claim waits for it and admits exactly what will land.
		copyReq.Transform = func(ctx context.Context, src *backend.GetObjectResult) (io.Reader, int64, func(), error) {
			out, err := req.Reform(ctx, src)
			if err != nil {
				return nil, 0, nil, err
			}
			intent.StoredForm, intent.SizeBytes = *out.Form, out.Size
			if err := w.ClaimCopyTarget(ctx, intent, req.Profile.Accepts); err != nil {
				out.Release()
				return nil, 0, nil, err
			}
			claimed, placement.Reformed = true, true
			return out.Body, out.Size, out.Release, nil
		}
	}
	if _, err := w.core.StreamCopy(ctx, copyReq); err != nil {
		if claimed {
			w.Discard(ctx, intent, req.Profile.Reasons.Orphan)
		}
		return 0, fmt.Errorf("stream copy %s -> %s: %w", srcName, req.DestName, err)
	}

	movedSize, err := w.Place(ctx, placement)
	if err != nil {
		return 0, fmt.Errorf("place copy %s -> %s: %w", srcName, req.DestName, err)
	}
	return movedSize, nil
}

// ClaimUploadTarget picks the backend a multipart upload will live on and
// records the upload there, setting params.BackendName to the backend that
// accepted it. Returns ErrInsufficientStorage when no backend is eligible, or
// the classified selection error.
//
// No bytes are claimed, since each part is counted by its own multipart_parts
// row as it arrives. The insert still decides, so a draining backend declines
// the upload and the next candidate is tried.
func (w *Coordinator) ClaimUploadTarget(ctx context.Context, span trace.Span, operation s3op.Operation, params *core.CreateMultipartUploadParams) (string, error) {
	eligible := w.core.EligibleForWrite([]s3op.Operation{operation}, 0, 0)
	if len(eligible) == 0 {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues(operation.String(), "write").Inc()
		observe.MarkSpanError(span, "usage limits exceeded on all backends")
		return "", core.ErrInsufficientStorage
	}
	name, err := w.claimFirst(eligible, func(name string) (bool, error) {
		params.BackendName = name
		return w.stores.CreateMultipartUpload(ctx, params)
	})
	if err != nil {
		return "", w.core.ClassifyWriteError(span, operation.String(), err)
	}
	return name, nil
}

// RankReplicaTargets orders the destinations a replication copy may go to,
// emptiest first under the same routing strategy a normal write uses, excluding
// backends that already hold a copy. An empty result means nothing is
// eligible. Room is decided by the conditional insert that records the copy,
// so a caller walks this list until one accepts.
func (w *Coordinator) RankReplicaTargets(size int64, exclusion map[string]bool) []string {
	eligible := w.core.EligibleForWrite([]s3op.Operation{s3op.PutObject}, 0, size)
	filtered := slices.DeleteFunc(slices.Clone(eligible), func(name string) bool {
		return exclusion[name]
	})
	if len(filtered) == 0 {
		return nil
	}
	return w.rankForWrite(w.core.Quota(), filtered)
}
