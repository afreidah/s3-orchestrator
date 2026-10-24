// -------------------------------------------------------------------------------
// Ops - Bulk Rewrite Paging and Outcome Tests
//
// Author: Alex Freidah
//
// How the driver classifies each move a pass makes, and the move it makes:
// a copy rewritten onto its own backend under the rewrite cleanup reasons.
//
// Every bulk rewrite pass walks a listing whose rows it is simultaneously
// rewriting, and each row it succeeds on falls out of the predicate that
// selected it. The set therefore shrinks underneath the walk, which is the one
// property these tests hold the driver to: it must see every row exactly once
// whether the rows it processes leave the set or stay in it.
//
// This is not hypothetical. Paging by offset over a shrinking set skipped a
// page for every page it processed, so a full-fleet decompress reported success
// having rewritten half the objects and left the rest encoded.
// -------------------------------------------------------------------------------

package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// pagingRows is how many rows these tests seed: enough to span several batches,
// since a paging fault is invisible inside a single page.
const pagingRows = 250

// shrinkingSet is a listing whose rows leave it as they are processed, which is
// what all four real bulk-rewrite listings do. Rows are held in key order and
// served from a cursor, exactly as the SQL does.
type shrinkingSet struct {
	mu      sync.Mutex
	rows    []core.ObjectLocation
	served  int
	limits  []int
	visited map[string]int
}

// newShrinkingSet seeds n rows under keys that sort in the order the listings
// return them.
func newShrinkingSet(n int) *shrinkingSet {
	s := &shrinkingSet{visited: map[string]int{}}
	for i := range n {
		s.rows = append(s.rows, core.ObjectLocation{
			ObjectKey:   fmt.Sprintf("obj-%04d", i),
			BackendName: "backend-a",
			SizeBytes:   1024,
		})
	}
	return s
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// page returns the rows after the cursor, up to limit. served bounds the total
// handed out so a driver that never advances its cursor fails the test instead
// of spinning forever.
func (s *shrinkingSet) page(_ context.Context, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.limits = append(s.limits, limit)
	var out []core.ObjectLocation
	for i := range s.rows {
		r := &s.rows[i]
		if r.ObjectKey > after.ObjectKey || (r.ObjectKey == after.ObjectKey && r.BackendName > after.BackendName) {
			out = append(out, *r)
			if len(out) == limit {
				break
			}
		}
	}
	s.served += len(out)
	if s.served > 4*len(s.rows)+4*pagingRows {
		return nil, fmt.Errorf("listing served %d rows for a set of %d: the pass is not advancing", s.served, len(s.rows))
	}
	return out, nil
}

// remove takes a row out of the set, standing in for the metadata update that
// makes a rewritten copy stop matching the listing's predicate.
func (s *shrinkingSet) remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = slicesDelete(s.rows, key)
}

// visit records that the pass considered a key, so a row served twice is caught
// as precisely as one never served at all.
func (s *shrinkingSet) visit(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.visited[key]++
}

// slicesDelete returns rows without the entry for key.
func slicesDelete(rows []core.ObjectLocation, key string) []core.ObjectLocation {
	out := rows[:0]
	for i := range rows {
		if rows[i].ObjectKey != key {
			out = append(out, rows[i])
		}
	}
	return out
}

// assertVisitedAll fails unless every seeded key was considered exactly once.
func (s *shrinkingSet) assertVisitedAll(t *testing.T, want int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()

	var missing, twice []string
	for i := range want {
		key := fmt.Sprintf("obj-%04d", i)
		switch n := s.visited[key]; {
		case n == 0:
			missing = append(missing, key)
		case n > 1:
			twice = append(twice, key)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d rows were never processed, starting at %s",
			len(missing), want, strings.Join(missing[:min(3, len(missing))], ", "))
	}
	if len(twice) > 0 {
		t.Errorf("%d rows were processed more than once, starting at %s",
			len(twice), strings.Join(twice[:min(3, len(twice))], ", "))
	}
}

// moverFunc adapts a function to the Mover interface, so a paging test states
// what each move does in one closure.
type moverFunc func(ctx context.Context, req *writepath.MoveRequest) (int64, error)

// MoveObject calls the function.
func (f moverFunc) MoveObject(ctx context.Context, req *writepath.MoveRequest) (int64, error) {
	return f(ctx, req)
}

// pagingEnv builds the collaborators the driver needs, over a mover that does
// what move says.
func pagingEnv(t *testing.T, move moverFunc) bulkRewriteEnv {
	t.Helper()
	return bulkRewriteEnv{
		log:    slog.New(slog.DiscardHandler),
		mover:  move,
		layers: &storedform.Layers{},
	}
}

// pagingCounter is a throwaway counter so these tests do not move the process
// metrics every other test in the package reads.
func pagingCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bulk_rewrite_paging_test"}, []string{"status"})
}

// leavingRewrite visits each row and takes it out of the set once its move
// succeeds, which is what a successful conversion does to a real listing.
func leavingRewrite(set *shrinkingSet) moverFunc {
	return func(_ context.Context, req *writepath.MoveRequest) (int64, error) {
		set.visit(req.Source.ObjectKey)
		set.remove(req.Source.ObjectKey)
		return req.Source.SizeBytes, nil
	}
}

// decliningRewrite visits each row and declines it, leaving it in the set as a
// copy the ratio turned away does.
func decliningRewrite(set *shrinkingSet) moverFunc {
	return func(_ context.Context, req *writepath.MoveRequest) (int64, error) {
		set.visit(req.Source.ObjectKey)
		return 0, &storedform.Declined{EncodedSize: req.Source.SizeBytes}
	}
}

// pagingOp is the bulk rewrite op every paging test drives over set.
func pagingOp(set *shrinkingSet, maxRewrites int) bulkRewriteOp {
	return bulkRewriteOp{
		opName:      "paging-test",
		resultLabel: "rewritten",
		counter:     pagingCounter(),
		listFn:      set.page,
		want:        func(loc *core.ObjectLocation) core.StoredFlags { return loc.Form() },
		maxRewrites: maxRewrites,
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestRunBulkRewrite_ProcessesEveryRowAsTheSetShrinks is the regression test for
// the paging fault. Every row here succeeds and therefore leaves the listing,
// which is the decompress-existing case: with an offset walk the pass stepped
// over a page for every page it completed and stopped early, reporting a clean
// run over roughly half the fleet.
func TestRunBulkRewrite_ProcessesEveryRowAsTheSetShrinks(t *testing.T) {
	t.Parallel()
	set := newShrinkingSet(pagingRows)

	res, err := pagingOp(set, 0).run(context.Background(), pagingEnv(t, leavingRewrite(set)), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}

	if res.Succeeded != pagingRows || res.Total != pagingRows {
		t.Errorf("counts = %+v, want %d succeeded out of %d", res, pagingRows, pagingRows)
	}
	set.assertVisitedAll(t, pagingRows)
}

// TestRunBulkRewrite_TerminatesWhenRowsStay verifies a pass finishes when
// declined rows stay in the listing, which a pass that re-queried from the
// start each time would serve forever.
func TestRunBulkRewrite_TerminatesWhenRowsStay(t *testing.T) {
	t.Parallel()
	set := newShrinkingSet(pagingRows)

	res, err := pagingOp(set, 0).run(context.Background(), pagingEnv(t, decliningRewrite(set)), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}

	if res.Skipped != pagingRows || res.Total != pagingRows {
		t.Errorf("counts = %+v, want %d skipped out of %d", res, pagingRows, pagingRows)
	}
	set.assertVisitedAll(t, pagingRows)
}

// TestRunBulkRewrite_MixedOutcomesStillCoverTheSet checks the two behaviours
// compose: a pass where some rows leave the set and others stay must still end
// having considered each exactly once. Every real compression pass is this
// shape, since the ratio threshold declines whatever it cannot shrink.
func TestRunBulkRewrite_MixedOutcomesStillCoverTheSet(t *testing.T) {
	t.Parallel()
	set := newShrinkingSet(pagingRows)

	// Declining every third row leaves the set interleaved rather than split,
	// so a cursor that skipped past a run of survivors would be caught.
	declined := func(key string) bool {
		var n int
		if _, err := fmt.Sscanf(key, "obj-%d", &n); err != nil {
			return false
		}
		return n%3 == 0
	}

	leave, decline := leavingRewrite(set), decliningRewrite(set)
	move := func(ctx context.Context, req *writepath.MoveRequest) (int64, error) {
		if declined(req.Source.ObjectKey) {
			return decline(ctx, req)
		}
		return leave(ctx, req)
	}
	res, err := pagingOp(set, 0).run(context.Background(), pagingEnv(t, move), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}

	if res.Total != pagingRows || res.Succeeded+res.Skipped != pagingRows {
		t.Errorf("counts = %+v, want %d rows all accounted for", res, pagingRows)
	}
	if res.Skipped == 0 || res.Succeeded == 0 {
		t.Errorf("counts = %+v, want both outcomes represented", res)
	}
	set.assertVisitedAll(t, pagingRows)
}

// TestRunBulkRewrite_DeclinesObjectsWithoutUsageHeadroom verifies an object
// the usage limits turn away is counted as skipped, not failed, and stays in
// the listing for a later run.
func TestRunBulkRewrite_DeclinesObjectsWithoutUsageHeadroom(t *testing.T) {
	t.Parallel()
	set := newShrinkingSet(3)
	move := func(_ context.Context, req *writepath.MoveRequest) (int64, error) {
		set.visit(req.Source.ObjectKey)
		return 0, fmt.Errorf("stream copy: %w", core.ErrUsageLimitExceeded)
	}

	res, err := pagingOp(set, 0).run(context.Background(), pagingEnv(t, move), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}

	if res.Skipped != 3 || res.Succeeded != 0 || res.Failed != 0 {
		t.Errorf("counts = %+v, want 3 skipped", res)
	}
	if len(set.rows) != 3 {
		t.Errorf("%d rows left in the set, want all 3", len(set.rows))
	}
	set.assertVisitedAll(t, 3)
}

// TestBulkRewritePageSize covers the arithmetic that turns a cap into a listing
// limit. The uncapped and nearly-exhausted ends are the obvious cases; the one
// that matters is a cap wider than a page, where asking for the remaining
// budget instead of a full page would page the fleet one row at a time.
func TestBulkRewritePageSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		maxRewrites int
		rewritten   int
		want        int
	}{
		{"uncapped", 0, 0, bulkRewriteBatchSize},
		{"negative cap reads as uncapped", -1, 0, bulkRewriteBatchSize},
		{"cap below a page", 3, 0, 3},
		{"cap exactly a page", bulkRewriteBatchSize, 0, bulkRewriteBatchSize},
		{"cap wider than a page", 250, 0, bulkRewriteBatchSize},
		{"budget still wider than a page", 250, 100, bulkRewriteBatchSize},
		{"budget narrowed to the remainder", 250, 200, 50},
		{"one row of budget left", 250, 249, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bulkRewritePageSize(tc.maxRewrites, tc.rewritten); got != tc.want {
				t.Errorf("bulkRewritePageSize(%d, %d) = %d, want %d",
					tc.maxRewrites, tc.rewritten, got, tc.want)
			}
		})
	}
}

// TestRunBulkRewrite_CapSpanningPagesStopsExactly drives a cap wider than one
// listing page, so the final page must narrow to the remaining budget or the
// pass overshoots.
func TestRunBulkRewrite_CapSpanningPagesStopsExactly(t *testing.T) {
	t.Parallel()
	const capRewrites = bulkRewriteBatchSize + 50
	set := newShrinkingSet(pagingRows)

	res, err := pagingOp(set, capRewrites).run(context.Background(), pagingEnv(t, leavingRewrite(set)), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}

	if res.Succeeded != capRewrites || res.Total != capRewrites {
		t.Errorf("counts = %+v, want exactly the %d asked for", res, capRewrites)
	}
	// The rows the cap did not reach must be untouched and still listed, which
	// is what lets the next run continue from here.
	if len(set.rows) != pagingRows-capRewrites {
		t.Errorf("%d rows left in the set, want %d", len(set.rows), pagingRows-capRewrites)
	}
	set.assertVisitedAll(t, capRewrites)

	// A full page first, then only the remaining budget. Asking for the full
	// batch on the second page would read 50 rows the pass never converts.
	if len(set.limits) < 2 || set.limits[0] != bulkRewriteBatchSize || set.limits[1] != 50 {
		t.Errorf("page sizes = %v, want the first %d then the remaining 50",
			set.limits, bulkRewriteBatchSize)
	}
}

// TestRunBulkRewrite_CapCountsRewritesNotRowsConsidered checks a cap spends its
// budget on conversions rather than on rows the pass declines. Compress-existing
// declines most of a fleet of media, so a cap charged per row considered would
// return having converted a handful and report itself finished.
func TestRunBulkRewrite_CapCountsRewritesNotRowsConsidered(t *testing.T) {
	t.Parallel()
	const capRewrites = 20
	set := newShrinkingSet(pagingRows)

	// Declining every other row means reaching the cap has to walk twice as
	// many rows as it converts.
	var considered int
	leave := leavingRewrite(set)
	move := func(ctx context.Context, req *writepath.MoveRequest) (int64, error) {
		considered++
		if considered%2 == 1 {
			return 0, &storedform.Declined{}
		}
		return leave(ctx, req)
	}
	res, err := pagingOp(set, capRewrites).run(context.Background(), pagingEnv(t, move), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}

	if res.Succeeded != capRewrites {
		t.Errorf("rewrote %d, want the %d asked for despite the declines", res.Succeeded, capRewrites)
	}
	if res.Skipped != capRewrites {
		t.Errorf("skipped %d, want the %d declined alongside them", res.Skipped, capRewrites)
	}
	if res.Total != 2*capRewrites {
		t.Errorf("considered %d rows, want %d: a cap counts conversions, not rows", res.Total, 2*capRewrites)
	}
}

// TestRunBulkRewrite_ClassifiesEachMoveOutcome drives one copy through each
// answer a move can give and checks where the pass tallies it and which
// counters move. A usage or target refusal is a skip, a copy a client replaced
// is changed, and anything else is a failure.
func TestRunBulkRewrite_ClassifiesEachMoveOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		err          error
		want         BulkRewriteResult
		label        string
		usageDecline float64
		copyChanged  float64
	}{
		{"placed", nil, BulkRewriteResult{Succeeded: 1, Total: 1}, "success", 0, 0},
		{"ratio declined", &storedform.Declined{EncodedSize: 900}, BulkRewriteResult{Skipped: 1, Total: 1}, "skipped", 0, 0},
		{"usage limits", fmt.Errorf("copy: %w", core.ErrUsageLimitExceeded), BulkRewriteResult{Skipped: 1, Total: 1}, "skipped", 1, 0},
		{"target refused", fmt.Errorf("place: %w", core.ErrTargetRefused), BulkRewriteResult{Skipped: 1, Total: 1}, "skipped", 0, 0},
		{"copy changed", fmt.Errorf("place: %w", core.ErrCopyChanged), BulkRewriteResult{Changed: 1, Total: 1}, "changed", 0, 1},
		{"failed", errors.New("backend unreachable"), BulkRewriteResult{Failed: 1, Total: 1}, "error", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			set := newShrinkingSet(1)
			op := pagingOp(set, 0)
			op.opName = "outcome-test-" + tc.name
			move := func(context.Context, *writepath.MoveRequest) (int64, error) { return 1024, tc.err }

			res, err := op.run(context.Background(), pagingEnv(t, move), nil)
			if err != nil {
				t.Fatalf("bulk rewrite: %v", err)
			}
			if res != tc.want {
				t.Errorf("counts = %+v, want %+v", res, tc.want)
			}
			assertOutcomeCounters(t, &op, tc.label, tc.usageDecline, tc.copyChanged)
		})
	}
}

// assertOutcomeCounters checks a pass moved its own result counter once under
// label and the usage-declined and copy-changed counters by the given amounts.
func assertOutcomeCounters(t *testing.T, op *bulkRewriteOp, label string, usageDecline, copyChanged float64) {
	t.Helper()
	if got := testutil.ToFloat64(op.counter.WithLabelValues(label)); got != 1 {
		t.Errorf("%s counter = %v, want 1", label, got)
	}
	if got := testutil.ToFloat64(telemetry.BulkRewriteUsageDeclinedTotal.WithLabelValues(op.opName)); got != usageDecline {
		t.Errorf("usage declined counter = %v, want %v", got, usageDecline)
	}
	if got := testutil.ToFloat64(telemetry.BulkRewriteCopyChangedTotal.WithLabelValues(op.opName)); got != copyChanged {
		t.Errorf("copy changed counter = %v, want %v", got, copyChanged)
	}
}

// TestRunBulkRewrite_DeclineReachesOnDeclined checks a ratio decline hands the
// copy and the encoded size to the pass's decline hook, and that a pass with
// no hook still skips the copy.
func TestRunBulkRewrite_DeclineReachesOnDeclined(t *testing.T) {
	t.Parallel()
	set := newShrinkingSet(1)
	var gotKey string
	var gotSize int64
	op := pagingOp(set, 0)
	op.onDeclined = func(_ context.Context, loc *core.ObjectLocation, encodedSize int64) {
		gotKey, gotSize = loc.ObjectKey, encodedSize
	}
	move := func(context.Context, *writepath.MoveRequest) (int64, error) {
		return 0, fmt.Errorf("stream copy: %w", &storedform.Declined{EncodedSize: 777})
	}

	res, err := op.run(context.Background(), pagingEnv(t, move), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}
	if res.Skipped != 1 {
		t.Errorf("counts = %+v, want one skipped", res)
	}
	if gotKey != "obj-0000" || gotSize != 777 {
		t.Errorf("onDeclined got (%q, %d), want (obj-0000, 777)", gotKey, gotSize)
	}
}

// TestRunBulkRewrite_AuditsAChangedCopy checks a copy a client replaced
// mid-pass is audited as a conversion race. Not parallel: the audit hook is
// process-wide.
func TestRunBulkRewrite_AuditsAChangedCopy(t *testing.T) {
	var raced atomic.Int64
	audit.SetOnEvent(func(event string) {
		if event == "storage.ConversionRaced" {
			raced.Add(1)
		}
	})
	t.Cleanup(func() { audit.SetOnEvent(nil) })

	set := newShrinkingSet(1)
	move := func(context.Context, *writepath.MoveRequest) (int64, error) { return 0, core.ErrCopyChanged }
	if _, err := pagingOp(set, 0).run(context.Background(), pagingEnv(t, move), nil); err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}
	if got := raced.Load(); got != 1 {
		t.Errorf("ConversionRaced events = %d, want 1", got)
	}
}

// TestRunBulkRewrite_MovesOntoTheSourceBackend checks each copy is handed to
// the mover as a rewrite: read from and written back to its own backend under
// the rewrite cleanup reasons, with a Reform that turns the copy into the form
// the pass wants.
func TestRunBulkRewrite_MovesOntoTheSourceBackend(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t)
	payload := []byte("hello world")
	be := &fakeBackend{payload: payload}
	mover := &fakeMover{backends: map[string]s3be.ObjectBackend{"backend-a": be}}
	env := bulkRewriteEnv{
		log:    slog.New(slog.DiscardHandler),
		mover:  mover,
		layers: &storedform.Layers{Encryptor: enc},
	}
	set := newShrinkingSet(1)
	set.rows[0].ContentHash = "hash"
	set.rows[0].SizeBytes = int64(len(payload))
	op := pagingOp(set, 0)
	op.want = func(loc *core.ObjectLocation) core.StoredFlags { return loc.Form() | core.StoredEncrypted }

	if _, err := op.run(context.Background(), env, nil); err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}
	reqs := mover.moveRequests()
	if len(reqs) != 1 {
		t.Fatalf("MoveObject calls = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.DestName != "backend-a" || req.Source.BackendName != "backend-a" {
		t.Errorf("move %s -> %s, want backend-a onto itself", req.Source.BackendName, req.DestName)
	}
	if req.Profile != writepath.RewriteMove {
		t.Errorf("profile = %+v, want RewriteMove", req.Profile)
	}
	placed := mover.placements()
	if len(placed) != 1 {
		t.Fatalf("placements = %d, want 1", len(placed))
	}
	got := placed[0]
	if !got.form.Encrypted || got.form.PlaintextSize != int64(len(payload)) || got.form.ContentHash != "hash" {
		t.Errorf("placed form = %+v, want encrypted over %d bytes carrying the hash", got.form, len(payload))
	}
	if got.size != int64(len(got.body)) || bytes.Equal(got.body, payload) {
		t.Errorf("placed %d bytes (size %d), want ciphertext of the payload", len(got.body), got.size)
	}
}
