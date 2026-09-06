// -------------------------------------------------------------------------------
// Object Manager - PUT Fan-Out
//
// Author: Alex Freidah
//
// Places an object's copies during the write instead of leaving every one after
// the first to the replicator, which would read the object back off a backend
// and pay that backend's egress to make each of them. The payload is already in
// hand and replayable, so a further copy costs one more upload and no read.
//
// Off unless an operator turns it on: the work is lower but it lands at write
// time rather than spread across replicator cycles. PutObject owns the
// sequential path this replaces, in put.go.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"go.opentelemetry.io/otel/trace"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// errFanoutUnavailable reports that the fleet is already carrying as many
// unfinished tails as it will, so this write places one copy instead. Handled
// by PutObject, which falls through to the sequential path; it never reaches a
// client, because nothing about it makes the write fail.
var errFanoutUnavailable = errors.New("no detached-upload slot available")

// uploadOutcome is one copy's upload reporting back: the intent it was claimed
// under, and whether the bytes reached the backend.
type uploadOutcome struct {
	intent *core.PendingObject
	err    error
}

// writeOutcome is what the client is answered with, handed to the request
// goroutine by whichever copy commits first.
type writeOutcome struct {
	backend string
	etag    string
	size    int64
	err     error
}

// copyFanout carries one write's fan-out across the goroutines that run it: the
// request goroutine that waits for a copy to commit, one goroutine per upload,
// and the commit goroutine that resolves them in the order they finish.
//
// Only the commit goroutine touches the intents after the uploads start, which
// is what lets the commits be ordered without a lock: the first copy to land
// records the object and every later one adds itself to it.
type copyFanout struct {
	mgr     *Manager
	req     *PutObjectRequest
	plan    *putPlan
	claimed []*core.PendingObject
	release func() // the tail's slot in the detached registry
	uploads chan uploadOutcome
	settled chan writeOutcome
}

// -------------------------------------------------------------------------
// FAN-OUT
// -------------------------------------------------------------------------

// putCopiesInParallel claims a backend per copy, uploads to all of them at
// once, and answers the client as soon as one copy is committed.
//
// It does not wait for the rest. Waiting would put the slowest backend on the
// critical path of every write, which is the thing placing one copy and
// repairing later exists to avoid, so the uploads still running carry on
// against a context that outlives the request and commit themselves as they
// finish. Whatever does not land is a shortfall the replicator fills on its
// next pass, which is what it does for every copy today.
func (o *Manager) putCopiesInParallel(ctx context.Context, span trace.Span, req *PutObjectRequest, plan *putPlan, eligible []string, start time.Time) (string, error) {
	const operation = s3op.PutObject

	// The slot is taken before anything is claimed, so a write that cannot have
	// one has not yet done anything it would need to undo. Refused means the
	// fleet is already carrying as many unfinished tails as it will: this write
	// places one copy and the replicator makes the rest, which costs the read
	// back that the fan-out exists to avoid and is why the ceiling is set where
	// a healthy fleet never reaches it.
	release, admitted := o.detached.Begin()
	if !admitted {
		telemetry.ReplicationWriteFanoutSkippedTotal.Inc()
		o.log.WarnContext(ctx, "no slot for the copies this write would place; leaving them to the replicator",
			"key", req.Key, "in_flight", o.detached.Depth())
		return "", errFanoutUnavailable
	}

	claimed, err := o.coord.ClaimWriteCopies(ctx, o.copyIntents(req, plan), eligible)
	if err != nil {
		release()
		return "", o.core.ClassifyWriteError(span, operation.String(), err)
	}
	span.SetAttributes(telemetry.AttrCopiesClaimed.Int(len(claimed)))

	f := &copyFanout{
		mgr:     o,
		req:     req,
		plan:    plan,
		claimed: claimed,
		release: release,
		uploads: make(chan uploadOutcome, len(claimed)),
		settled: make(chan writeOutcome, 1),
	}

	// Detached from the request: the response is sent on the first commit, and
	// cancelling the rest at that point would abandon copies that are nearly
	// done. Each upload holds the payload so the body outlives the handler that
	// materialized it.
	detached := context.WithoutCancel(ctx)
	for _, p := range claimed {
		plan.hold()
		go f.upload(detached, p)
	}
	go f.commitAsTheyLand(detached, span)

	out := <-f.settled
	if out.err != nil {
		observe.RecordSpanError(span, out.err)
		return "", out.err
	}
	o.finalizePutSuccess(ctx, span, operation, req.Key, out.backend, out.size, start, nil)
	return out.etag, nil
}

// copyIntents builds one intent per copy the write will place. The first is the
// primary, which is what a reaper promotes if this process dies before anything
// commits; the rest are companions, which it discards, because bytes on a
// backend no copy was ever recorded on cannot be told apart from an older
// object at the same path.
//
// The role says what an abandoned intent means, not which copy records the
// object. That is whichever upload lands first, so a companion's intent
// routinely commits the object and the primary's routinely adds itself to it.
func (o *Manager) copyIntents(req *PutObjectRequest, plan *putPlan) []*core.PendingObject {
	identity := putIdentity(plan.etagDigest, req)
	intents := make([]*core.PendingObject, o.copiesPerWrite)
	for i := range intents {
		intents[i] = writepath.NewPendingIntent(req.Key, plan.uploadSize, plan.form, identity)
		if i > 0 {
			intents[i].Role = core.PendingRoleCompanion
		}
	}
	return intents
}

// upload sends one copy and reports what happened. The payload hold it releases
// is the one taken before the goroutine started, so the body survives exactly
// as long as something is still reading it.
func (f *copyFanout) upload(ctx context.Context, p *core.PendingObject) {
	defer f.plan.cleanup()
	f.uploads <- uploadOutcome{intent: p, err: f.mgr.uploadCopy(ctx, f.req, f.plan, p)}
}

// uploadCopy puts one copy's bytes on its backend, replaying the shared
// payload. A drain that started after the backend was claimed is caught the
// same way the sequential path catches it, before anything is recorded.
func (o *Manager) uploadCopy(ctx context.Context, req *PutObjectRequest, plan *putPlan, p *core.PendingObject) error {
	be, err := o.core.GetBackend(p.BackendName)
	if err != nil {
		return err
	}
	body, err := plan.body.Reader()
	if err != nil {
		return err
	}
	bctx, bcancel := o.core.WithTimeout(ctx)
	_, err = be.PutObject(bctx, req.Key, body, plan.uploadSize, req.ContentType, req.Metadata)
	bcancel()
	if err != nil {
		o.core.Acct().APICall(s3op.PutObject, p.BackendName)
		return err
	}
	if o.core.IsDraining(p.BackendName) {
		telemetry.DrainRaceAbortedTotal.Inc()
		o.coord.RecoverFromRecordFailure(ctx, be, p.BackendName, req.Key, "drain_race_aborted", plan.uploadSize)
		return errDrainRaceAborted
	}
	return nil
}

// commitAsTheyLand resolves each copy in the order its upload finishes. The
// first to land records the object and answers the client; the rest add
// themselves to what it recorded.
//
// A commit that fails takes the write with it and abandons the copies still
// running, rather than leaving them to record themselves against a key nothing
// anchors. Their intents stay, and the reaper resolves them the way it resolves
// any companion whose write did not finish.
func (f *copyFanout) commitAsTheyLand(ctx context.Context, span trace.Span) {
	// Released once every copy has settled, which is the moment this write is
	// no longer something a shutdown has to wait for.
	defer f.release()

	live := f.liveIntents()
	committed, abandoned := false, false
	var lastErr error

	for range f.claimed {
		out := <-f.uploads
		delete(live, out.intent.IntentID)
		switch {
		case out.err != nil:
			lastErr = out.err
			f.uploadFailed(ctx, out)
		case abandoned:
			f.leaveForReaper(ctx, out.intent, "an earlier copy's commit failed")
		case committed:
			_ = f.mgr.coord.CommitCompanionCopy(ctx, out.intent)
		default:
			err := f.commitFirstCopy(ctx, span, out.intent, live)
			committed, abandoned = err == nil, err != nil
			lastErr = err
		}
	}
	// Nothing committed and nothing abandoned means every upload failed, and
	// the client is still waiting. An abandoned write was already answered by
	// the commit that failed.
	if !committed && !abandoned {
		f.settle(writeOutcome{err: lastErr})
	}
}

// commitFirstCopy records the object from the copy that landed first and
// answers the client with it.
//
// The copies still uploading ride along as Placing. That keeps their intents,
// which every other intent for the key does not get, and it keeps their
// backends out of the displacement this commit performs: an overwrite deleting
// the previous copy from a backend this write is still uploading to would
// delete the bytes landing there.
func (f *copyFanout) commitFirstCopy(ctx context.Context, span trace.Span, p *core.PendingObject, live map[string]*core.PendingObject) error {
	err := f.mgr.coord.RecordObjectAndPromoteIntent(ctx, span, &core.RecordObjectRequest{
		Key:      f.req.Key,
		Size:     f.plan.uploadSize,
		Form:     f.plan.form,
		Identity: p.Identity,
		Tags:     f.req.Tags,
		Copies:   []core.ObjectCopy{{Backend: p.BackendName, IntentID: p.IntentID}},
		Placing:  placingCopies(live),
	})
	if err != nil {
		f.settle(writeOutcome{err: f.mgr.core.ClassifyWriteError(span, s3op.PutObject.String(), err)})
		return err
	}
	// Not counted as a placed copy: this one is the object, and the metric
	// measures the copies that would otherwise have been the replicator's.
	span.SetAttributes(telemetry.AttrBackendName.String(p.BackendName))
	f.settle(writeOutcome{backend: p.BackendName, etag: p.Identity.ETag, size: f.plan.uploadSize})
	return nil
}

// uploadFailed records a copy whose bytes never reached its backend. The intent
// is left alone: a backend error does not reliably mean the object is absent,
// so whether those bytes exist is settled by whoever resolves the intent - the
// commit that supersedes it, or the reaper's probe.
func (f *copyFanout) uploadFailed(ctx context.Context, out uploadOutcome) {
	telemetry.ReplicationWriteCopiesTotal.WithLabelValues(writepath.WriteCopyFailed).Inc()
	f.mgr.log.WarnContext(ctx, "a copy of the write failed on its backend",
		"key", f.req.Key, "backend", out.intent.BackendName,
		"intent_id", out.intent.IntentID, logfmt.Err(out.err))
}

// leaveForReaper gives up on a copy that uploaded successfully but has nothing
// to attach itself to. The intent stays, and because it is a companion the
// reaper never promotes it: it removes the row and takes the bytes with it.
func (f *copyFanout) leaveForReaper(ctx context.Context, p *core.PendingObject, why string) {
	telemetry.ReplicationWriteCopiesTotal.WithLabelValues(writepath.WriteCopyFailed).Inc()
	f.mgr.log.WarnContext(ctx, "leaving a copy's intent for the reaper",
		"key", f.req.Key, "backend", p.BackendName, "intent_id", p.IntentID, "reason", why)
}

// settle answers the request goroutine. The channel is buffered and written
// once, so this never blocks and a caller that has already returned cannot
// strand the commits that follow.
func (f *copyFanout) settle(out writeOutcome) {
	f.settled <- out
}

// liveIntents indexes the claimed intents by id so the commit goroutine can
// strike each one off as its upload reports.
func (f *copyFanout) liveIntents() map[string]*core.PendingObject {
	live := make(map[string]*core.PendingObject, len(f.claimed))
	for _, p := range f.claimed {
		live[p.IntentID] = p
	}
	return live
}

// placingCopies names the copies still uploading, which a commit both keeps the
// intents of and holds back from displacement.
func placingCopies(live map[string]*core.PendingObject) []core.ObjectCopy {
	copies := make([]core.ObjectCopy, 0, len(live))
	for _, p := range live {
		copies = append(copies, core.ObjectCopy{Backend: p.BackendName, IntentID: p.IntentID})
	}
	return copies
}
