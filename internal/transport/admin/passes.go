// -------------------------------------------------------------------------------
// Admin API - On-Demand Passes
//
// Author: Alex Freidah
//
// The passes an operator runs on demand, each defined once for every
// transport. A Pass runs its operation, settles how it ended and renders the
// adminapi response for it. The admin API serves it as JSON or as an NDJSON
// stream; the dashboard runs it in the background and answers its status poll
// with the same response.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Pass is one on-demand pass. verb prefixes each streamed item line, and
// sequential streams each item as a live prefix rather than a finished line.
// failMsg is what a caller is told of a failure, which is logged in full.
// skipIsError fails a skipped pass with its reason, for passes whose skip
// means the request cannot be served as asked.
//
// body renders the response and is also the stream's result fields, so the
// modes cannot report different keys; it receives the outcome so a skipped
// pass renders the same shape with zero counts. summary is the stream's
// result line.
type Pass[R any] struct {
	op          string
	verb        string
	sequential  bool
	failMsg     string
	skipIsError bool
	run         func(context.Context, progress.Observer) (R, error)
	body        func(adminapi.Outcome, R) any
	summary     func(R) (processed int, line string)
}

// Report is how one pass run ended, decided once for every transport. Result
// is an adminstream outcome. Message is the skip reason or what the caller is
// told of a failure, and Status the code a failed JSON response carries. Body
// is the pass's response, set when the pass ran or was skipped.
type Report struct {
	Result  string
	Message string
	Status  int
	Body    any
}

// BackfillParams are the knobs of a checksum backfill: objects per batch, the
// cap on objects this run hashes, the pause between batches and the backend it
// is limited to. Zero values mean the defaults, no cap, no pause and every
// backend.
type BackfillParams struct {
	BatchSize  int
	MaxObjects int
	Pause      time.Duration
	Backend    string
}

// Rewrite selects one of the four bulk rewrites.
type Rewrite int

// The bulk rewrites.
const (
	CompressExisting Rewrite = iota
	DecompressExisting
	EncryptExisting
	DecryptExisting
)

// rewriteSpec is what tells the bulk rewrites apart: the operation and its
// stream verb, what a failure is called, the ops pass it runs and the
// response that names its success count.
type rewriteSpec struct {
	op      string
	verb    string
	failMsg string
	run     func(*ops.Services) func(context.Context, progress.Observer, int, string) (ops.BulkRewriteResult, error)
	body    func(adminapi.BulkRewriteOutcome, int) any
}

// rewrites holds each bulk rewrite's spec, indexed by Rewrite.
var rewrites = [...]rewriteSpec{
	CompressExisting: {
		op: "compress-existing", verb: "compressing", failMsg: "failed to list uncompressed objects",
		run: func(s *ops.Services) func(context.Context, progress.Observer, int, string) (ops.BulkRewriteResult, error) {
			return s.Compression.CompressExisting
		},
		body: func(o adminapi.BulkRewriteOutcome, n int) any {
			return adminapi.CompressExistingResponse{BulkRewriteOutcome: o, Compressed: n}
		},
	},
	DecompressExisting: {
		op: "decompress-existing", verb: "decompressing", failMsg: "failed to list compressed objects",
		run: func(s *ops.Services) func(context.Context, progress.Observer, int, string) (ops.BulkRewriteResult, error) {
			return s.Compression.DecompressExisting
		},
		body: func(o adminapi.BulkRewriteOutcome, n int) any {
			return adminapi.DecompressExistingResponse{BulkRewriteOutcome: o, Decompressed: n}
		},
	},
	EncryptExisting: {
		op: "encrypt-existing", verb: "encrypting", failMsg: "failed to list unencrypted objects",
		run: func(s *ops.Services) func(context.Context, progress.Observer, int, string) (ops.BulkRewriteResult, error) {
			return s.Encryption.EncryptExisting
		},
		body: func(o adminapi.BulkRewriteOutcome, n int) any {
			return adminapi.EncryptExistingResponse{BulkRewriteOutcome: o, Encrypted: n}
		},
	},
	DecryptExisting: {
		op: "decrypt-existing", verb: "decrypting", failMsg: "failed to list encrypted objects",
		run: func(s *ops.Services) func(context.Context, progress.Observer, int, string) (ops.BulkRewriteResult, error) {
			return s.Encryption.DecryptExisting
		},
		body: func(o adminapi.BulkRewriteOutcome, n int) any {
			return adminapi.DecryptExistingResponse{BulkRewriteOutcome: o, Decrypted: n}
		},
	},
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Name returns the operation the pass runs, which also names it to the
// dashboard's background tracker.
func (p *Pass[R]) Name() string { return p.op }

// Run runs the pass without progress and reports how it ended, for a
// transport that wants only the final response. A failure is logged to log.
func (p *Pass[R]) Run(ctx context.Context, log *slog.Logger) Report {
	rep, _ := p.exec(ctx, log, nil)
	return rep
}

// RebalancePass runs one rebalance cycle. Moves run concurrently, so each
// streamed line is emitted on completion.
func RebalancePass(svc *ops.Services) *Pass[batch.Summary] {
	return &Pass[batch.Summary]{
		op:      "rebalance",
		verb:    "moving",
		failMsg: "rebalance failed",
		run:     svc.Rebalance.Run,
		body: func(o adminapi.Outcome, res batch.Summary) any {
			return adminapi.RebalanceResponse{Outcome: o, Moved: res.Succeeded}
		},
		summary: func(res batch.Summary) (int, string) {
			return res.Succeeded, fmt.Sprintf("moved %d objects", res.Succeeded)
		},
	}
}

// ReplicatePass runs one replication cycle. It fans objects out across a
// worker pool, so streamed lines are complete rather than live prefixes.
func ReplicatePass(svc *ops.Services) *Pass[worker.ReplicationSummary] {
	return &Pass[worker.ReplicationSummary]{
		op:      "replicate",
		verb:    "replicating",
		failMsg: "replication failed",
		run:     svc.Replication.Replicate,
		body: func(o adminapi.Outcome, res worker.ReplicationSummary) any {
			return adminapi.ReplicateResponse{Outcome: o, CopiesCreated: res.CopiesCreated, Failed: res.Failed}
		},
		summary: func(res worker.ReplicationSummary) (int, string) {
			return res.CopiesCreated, fmt.Sprintf("created %d copies", res.CopiesCreated)
		},
	}
}

// CleanExcessPass removes surplus copies of over-replicated objects, batchSize
// at a time (0 for the default). It fans out across a worker pool.
func CleanExcessPass(svc *ops.Services, batchSize int) *Pass[worker.OverReplicationSummary] {
	return &Pass[worker.OverReplicationSummary]{
		op:      "over-replication",
		verb:    "removing",
		failMsg: "over-replication cleanup failed",
		run: func(ctx context.Context, obs progress.Observer) (worker.OverReplicationSummary, error) {
			return svc.Replication.CleanExcess(ctx, batchSize, obs)
		},
		body: func(o adminapi.Outcome, res worker.OverReplicationSummary) any {
			return adminapi.OverReplicationCleanResponse{Outcome: o, CopiesRemoved: res.CopiesRemoved, Failed: res.Failed}
		},
		summary: func(res worker.OverReplicationSummary) (int, string) {
			return res.CopiesRemoved, fmt.Sprintf("removed %d copies", res.CopiesRemoved)
		},
	}
}

// LifecyclePass applies every lifecycle rule once. Objects are deleted one at
// a time, so each streamed line is a live prefix completed by its status.
func LifecyclePass(svc *ops.Services) *Pass[batch.Summary] {
	return &Pass[batch.Summary]{
		op:         "lifecycle",
		verb:       "expiring",
		sequential: true,
		failMsg:    "lifecycle sweep failed",
		run:        svc.Lifecycle.Run,
		body: func(o adminapi.Outcome, res batch.Summary) any {
			return adminapi.LifecycleResponse{Outcome: o, Deleted: res.Succeeded, Failed: res.Failed}
		},
		summary: func(res batch.Summary) (int, string) {
			return res.Succeeded, fmt.Sprintf("expired %d objects", res.Succeeded)
		},
	}
}

// ScrubPass runs one verification pass of batchSize copies (0 for the
// default), limited to backend when one is named. A copy it read counts as
// checked and one it could not decode as unreadable.
func ScrubPass(svc *ops.Services, batchSize int, backend string) *Pass[batch.Summary] {
	return &Pass[batch.Summary]{
		op:         "scrub",
		verb:       "verifying",
		sequential: true,
		failMsg:    "scrub failed",
		run: func(ctx context.Context, obs progress.Observer) (batch.Summary, error) {
			return svc.Integrity.Scrub(ctx, batchSize, backend, obs)
		},
		body: func(o adminapi.Outcome, res batch.Summary) any {
			return adminapi.ScrubResponse{
				Outcome:    o,
				Checked:    res.Attempted,
				Failed:     res.Failed,
				Unreadable: res.Skipped,
				Deferred:   res.Deferred,
			}
		},
		summary: func(res batch.Summary) (int, string) {
			return res.Attempted, fmt.Sprintf("checked %d, failed %d, unreadable %d, deferred %d",
				res.Attempted, res.Failed, res.Skipped, res.Deferred)
		},
	}
}

// BackfillPass fills the content hash of every object that lacks one, as p
// directs.
func BackfillPass(svc *ops.Services, p BackfillParams) *Pass[worker.BackfillResult] {
	return &Pass[worker.BackfillResult]{
		op:         "backfill-checksums",
		verb:       "hashing",
		sequential: true,
		failMsg:    "backfill failed",
		run: func(ctx context.Context, obs progress.Observer) (worker.BackfillResult, error) {
			return svc.Integrity.BackfillChecksums(ctx, p.BatchSize, p.MaxObjects, p.Pause, p.Backend, obs)
		},
		body: func(o adminapi.Outcome, res worker.BackfillResult) any {
			return adminapi.BackfillChecksumsResponse{
				Outcome:    o,
				Processed:  res.Succeeded,
				Unreadable: res.Skipped,
				Done:       res.Done,
			}
		},
		summary: func(res worker.BackfillResult) (int, string) {
			return res.Succeeded, fmt.Sprintf("hashed %d, unreadable %d", res.Succeeded, res.Skipped)
		},
	}
}

// RewritePass runs one bulk rewrite over every copy its listing selects,
// rewriting at most maxObjects (0 for all) and limited to backend when one is
// named. A skip means the encryptor or codec is not configured, which is the
// caller's to fix, so it fails the pass with its reason.
func RewritePass(svc *ops.Services, kind Rewrite, maxObjects int, backend string) *Pass[ops.BulkRewriteResult] {
	spec := rewrites[kind]
	run := spec.run(svc)
	return &Pass[ops.BulkRewriteResult]{
		op:          spec.op,
		verb:        spec.verb,
		sequential:  true,
		failMsg:     spec.failMsg,
		skipIsError: true,
		run: func(ctx context.Context, obs progress.Observer) (ops.BulkRewriteResult, error) {
			return run(ctx, obs, maxObjects, backend)
		},
		body: func(o adminapi.Outcome, res ops.BulkRewriteResult) any {
			tally := adminapi.BulkRewriteTally{Skipped: res.Skipped, Changed: res.Changed, Failed: res.Failed, Total: res.Total}
			return spec.body(adminapi.BulkRewriteOutcome{Outcome: o, BulkRewriteTally: tally}, res.Succeeded)
		},
		summary: func(res ops.BulkRewriteResult) (int, string) {
			return res.Succeeded, fmt.Sprintf("rewrote %d, skipped %d, changed %d, failed %d, of %d",
				res.Succeeded, res.Skipped, res.Changed, res.Failed, res.Total)
		},
	}
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// exec runs the pass with obs and settles how it ended, returning the raw
// result as well for the stream's summary line. A failure is logged here, so
// the caller is told only failMsg; a skip the pass refuses fails with its
// reason, which is the caller's to act on.
func (p *Pass[R]) exec(ctx context.Context, log *slog.Logger, obs progress.Observer) (Report, R) {
	res, err := p.run(ctx, obs)
	reason, skipped := ops.SkipReason(err)
	switch {
	case skipped && p.skipIsError:
		return Report{Result: adminstream.OutcomeFailed, Message: reason, Status: http.StatusBadRequest}, res
	case skipped:
		var zero R
		outcome := adminapi.Outcome{Status: adminstream.OutcomeSkipped, Reason: reason}
		return Report{Result: adminstream.OutcomeSkipped, Message: reason, Body: p.body(outcome, zero)}, res
	case err != nil:
		log.ErrorContext(ctx, p.failMsg, "error", err)
		return Report{Result: adminstream.OutcomeFailed, Message: p.failMsg, Status: http.StatusInternalServerError}, res
	}
	return Report{Result: adminstream.OutcomeOK, Body: p.body(adminapi.Outcome{Status: adminstream.OutcomeOK}, res)}, res
}
