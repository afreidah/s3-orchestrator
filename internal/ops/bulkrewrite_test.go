// -------------------------------------------------------------------------------
// Ops - Bulk Rewrite Paging and Outcome Tests
//
// Author: Alex Freidah
//
// How the driver classifies each copy's rewrite, which of an object's copies
// it hands the rewriter together, how it reports an object as one step, and
// what a capped pass counts.
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// pagingRows is how many rows these tests seed: enough to span several batches,
// since a paging fault is invisible inside a single page.
const pagingRows = 250

// shrinkingSet is a listing whose rows leave it as they are processed, which is
// what all four real bulk-rewrite listings do. Rows are held in key order and
// served from a cursor, exactly as the SQL does. failAfter, when positive,
// fails every listing after that many pages.
type shrinkingSet struct {
	mu        sync.Mutex
	rows      []core.ObjectLocation
	served    int
	pages     int
	failAfter int
	visited   map[string]int
}

// newShrinkingSet seeds n rows under keys that sort in the order the listings
// return them, one copy each.
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

// ListCopiesByForm returns the rows after the cursor, up to limit, whatever
// the filter. served bounds the total handed out so a driver that never
// advances its cursor fails the test instead of spinning forever.
func (s *shrinkingSet) ListCopiesByForm(_ context.Context, _ *core.FormFilter, limit int, after core.Cursor) ([]core.ObjectLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pages++
	if s.failAfter > 0 && s.pages > s.failAfter {
		return nil, errors.New("listing unavailable")
	}
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

// UpdateCopyMetadata is unused by the driver, which records through the
// rewriter.
func (s *shrinkingSet) UpdateCopyMetadata(context.Context, *core.ObjectLocation, *core.CopyPatch) error {
	return nil
}

// remove takes a key's rows out of the set, standing in for the metadata
// update that makes a rewritten copy stop matching the listing's predicate.
func (s *shrinkingSet) remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = slices.DeleteFunc(s.rows, func(r core.ObjectLocation) bool { return r.ObjectKey == key })
}

// visit records that the pass considered a key, so a row served twice is caught
// as precisely as one never served at all.
func (s *shrinkingSet) visit(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.visited[key]++
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

// rewriterFunc adapts a function to the Rewriter interface, so a paging test
// states what each rewrite does in one closure.
type rewriterFunc func(ctx context.Context, req *writepath.RewriteRequest) []error

// RewriteCopies calls the function.
func (f rewriterFunc) RewriteCopies(ctx context.Context, req *writepath.RewriteRequest) []error {
	return f(ctx, req)
}

// eachCopy is a rewriter whose answer for every copy is what fn returns for it.
func eachCopy(fn func(loc *core.ObjectLocation) error) rewriterFunc {
	return func(_ context.Context, req *writepath.RewriteRequest) []error {
		errs := make([]error, len(req.Copies))
		for i := range req.Copies {
			errs[i] = fn(&req.Copies[i])
		}
		return errs
	}
}

// answering is a rewriter that answers every copy with err.
func answering(err error) rewriterFunc {
	return eachCopy(func(*core.ObjectLocation) error { return err })
}

// pagingEnv builds the collaborators the driver needs, listing from set over a
// rewriter that does what rewrite says.
func pagingEnv(t *testing.T, set *shrinkingSet, rewrite Rewriter) bulkRewriteEnv {
	t.Helper()
	return bulkRewriteEnv{
		log:      slog.New(slog.DiscardHandler),
		store:    set,
		rewriter: rewrite,
		locker:   testx.Locker{},
		layers:   &storedform.Layers{},
	}
}

// pagingCounter is a throwaway counter so these tests do not move the process
// metrics every other test in the package reads.
func pagingCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bulk_rewrite_paging_test"}, []string{"status"})
}

// leavingRewrite visits each copy and takes it out of the set once rewritten,
// which is what a successful conversion does to a real listing.
func leavingRewrite(set *shrinkingSet) rewriterFunc {
	return eachCopy(func(loc *core.ObjectLocation) error {
		set.visit(loc.ObjectKey)
		set.remove(loc.ObjectKey)
		return nil
	})
}

// decliningRewrite visits each copy and declines it, leaving it in the set as
// a copy the ratio turned away does.
func decliningRewrite(set *shrinkingSet) rewriterFunc {
	return eachCopy(func(loc *core.ObjectLocation) error {
		set.visit(loc.ObjectKey)
		return &storedform.Declined{EncodedSize: loc.SizeBytes}
	})
}

// pagingOp is the bulk rewrite op every paging test drives. It changes no
// layer, so each copy is rewritten in the form it already has.
func pagingOp(maxObjects int) *bulkRewriteOp {
	return &bulkRewriteOp{
		opName:      "paging-test",
		resultLabel: "rewritten",
		counter:     pagingCounter(),
		maxObjects:  maxObjects,
	}
}

// copyOf is one copy of key on backend carrying hash, for the grouping tests.
func copyOf(key, backend, hash string) core.ObjectLocation {
	return core.ObjectLocation{ObjectKey: key, BackendName: backend, SizeBytes: 1024, StoredForm: core.StoredForm{ContentHash: hash}}
}

// recordingRewrite records the copies of each request it is handed, by
// backend, and rewrites them all.
func recordingRewrite(got *[][]string) rewriterFunc {
	return func(_ context.Context, req *writepath.RewriteRequest) []error {
		var names []string
		for i := range req.Copies {
			names = append(names, req.Copies[i].ObjectKey+"@"+req.Copies[i].BackendName)
		}
		*got = append(*got, names)
		return make([]error, len(req.Copies))
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

	res, err := pagingOp(0).run(context.Background(), pagingEnv(t, set, leavingRewrite(set)), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}

	if res.Succeeded != pagingRows || res.Total != pagingRows || res.Objects != pagingRows {
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

	res, err := pagingOp(0).run(context.Background(), pagingEnv(t, set, decliningRewrite(set)), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}

	if res.Skipped != pagingRows || res.Total != pagingRows || res.Objects != 0 {
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
	rewrite := func(ctx context.Context, req *writepath.RewriteRequest) []error {
		if declined(req.Copies[0].ObjectKey) {
			return decline(ctx, req)
		}
		return leave(ctx, req)
	}
	res, err := pagingOp(0).run(context.Background(), pagingEnv(t, set, rewriterFunc(rewrite)), nil)
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
	rewrite := eachCopy(func(loc *core.ObjectLocation) error {
		set.visit(loc.ObjectKey)
		return fmt.Errorf("read copy: %w", core.ErrUsageLimitExceeded)
	})

	res, err := pagingOp(0).run(context.Background(), pagingEnv(t, set, rewrite), nil)
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

// TestRunBulkRewrite_CapSpanningPagesStopsExactly drives a cap wider than one
// listing page: the pass stops on the object that reaches it, and the copies
// it listed beyond that stay in the set for the next run.
func TestRunBulkRewrite_CapSpanningPagesStopsExactly(t *testing.T) {
	t.Parallel()
	const capObjects = bulkRewriteBatchSize + 50
	set := newShrinkingSet(pagingRows)

	res, err := pagingOp(capObjects).run(context.Background(), pagingEnv(t, set, leavingRewrite(set)), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}

	if res.Objects != capObjects || res.Succeeded != capObjects || res.Total != capObjects {
		t.Errorf("counts = %+v, want exactly the %d asked for", res, capObjects)
	}
	if len(set.rows) != pagingRows-capObjects {
		t.Errorf("%d rows left in the set, want %d", len(set.rows), pagingRows-capObjects)
	}
	set.assertVisitedAll(t, capObjects)
}

// TestRunBulkRewrite_CapCountsObjectsRewrittenNotConsidered checks a cap
// spends its budget on conversions rather than on objects the pass declines.
// Compress-existing declines most of a fleet of media, so a cap charged per
// object considered would return having converted a handful and report itself
// finished.
func TestRunBulkRewrite_CapCountsObjectsRewrittenNotConsidered(t *testing.T) {
	t.Parallel()
	const capObjects = 20
	set := newShrinkingSet(pagingRows)

	// Declining every other object means reaching the cap has to walk twice as
	// many as it converts.
	var considered int
	leave := leavingRewrite(set)
	rewrite := func(ctx context.Context, req *writepath.RewriteRequest) []error {
		considered++
		if considered%2 == 1 {
			return []error{&storedform.Declined{}}
		}
		return leave(ctx, req)
	}
	res, err := pagingOp(capObjects).run(context.Background(), pagingEnv(t, set, rewriterFunc(rewrite)), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}

	if res.Objects != capObjects || res.Succeeded != capObjects {
		t.Errorf("counts = %+v, want the %d asked for despite the declines", res, capObjects)
	}
	if res.Skipped != capObjects || res.Total != 2*capObjects {
		t.Errorf("counts = %+v, want %d declined alongside them: a cap counts conversions", res, capObjects)
	}
}

// TestRunBulkRewrite_GroupsCopiesThatHoldTheSameBytes verifies an object's
// copies with the same layers and content hash go to the rewriter together,
// while a copy with another hash, no hash, other layers, or no management goes
// on its own.
func TestRunBulkRewrite_GroupsCopiesThatHoldTheSameBytes(t *testing.T) {
	t.Parallel()
	compressed := copyOf("k3", "b", "h")
	compressed.CompressionAlgorithm = "zstd"
	unmanaged := copyOf("k4", "b", "h")
	unmanaged.Unmanaged = true
	set := &shrinkingSet{visited: map[string]int{}, rows: []core.ObjectLocation{
		copyOf("k1", "a", "h"), copyOf("k1", "b", "h"), copyOf("k1", "c", "h"),
		copyOf("k2", "a", "h"), copyOf("k2", "b", "other"),
		copyOf("k3", "a", "h"), compressed,
		copyOf("k4", "a", "h"), unmanaged,
		copyOf("k5", "a", ""), copyOf("k5", "b", ""),
	}}
	var got [][]string

	res, err := pagingOp(0).run(context.Background(), pagingEnv(t, set, recordingRewrite(&got)), nil)
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}
	want := [][]string{
		{"k1@a", "k1@b", "k1@c"},
		{"k2@a"}, {"k2@b"},
		{"k3@a"}, {"k3@b"},
		{"k4@a"}, {"k4@b"},
		{"k5@a"}, {"k5@b"},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("requests = %v, want %v", got, want)
	}
	if res.Objects != 5 || res.Succeeded != 11 {
		t.Errorf("counts = %+v, want 5 objects and 11 copies", res)
	}
}

// TestRunBulkRewrite_ObjectSpanningPagesIsOneRequest verifies an object whose
// copies straddle a page boundary is still rewritten from one request.
func TestRunBulkRewrite_ObjectSpanningPagesIsOneRequest(t *testing.T) {
	t.Parallel()
	set := newShrinkingSet(bulkRewriteBatchSize - 1)
	set.rows = append(set.rows, copyOf("zz", "a", "h"), copyOf("zz", "b", "h"))
	var got [][]string

	if _, err := pagingOp(0).run(context.Background(), pagingEnv(t, set, recordingRewrite(&got)), nil); err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}
	if last := got[len(got)-1]; !slices.Equal(last, []string{"zz@a", "zz@b"}) {
		t.Errorf("last request = %v, want both copies of zz together", last)
	}
}

// TestRunBulkRewrite_ObjectStepReportsTheWorstCopy verifies each object is
// one progress step whose status is the worst of its copies, while the tally
// still counts each copy.
func TestRunBulkRewrite_ObjectStepReportsTheWorstCopy(t *testing.T) {
	t.Parallel()
	set := &shrinkingSet{visited: map[string]int{}, rows: []core.ObjectLocation{copyOf("k", "a", "h"), copyOf("k", "b", "h")}}
	rewrite := rewriterFunc(func(context.Context, *writepath.RewriteRequest) []error {
		return []error{nil, errors.New("write failed")}
	})
	var steps []progress.Step

	res, err := pagingOp(0).run(context.Background(), pagingEnv(t, set, rewrite), func(s progress.Step) { steps = append(steps, s) })
	if err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}
	if len(steps) != 2 || steps[0].Label != "k" || steps[1].Status != progress.StatusFailed {
		t.Errorf("steps = %+v, want one failed step for k", steps)
	}
	if res.Objects != 1 || res.Succeeded != 1 || res.Failed != 1 {
		t.Errorf("counts = %+v, want the object rewritten with one copy failed", res)
	}
}

// TestRunBulkRewrite_ListingFailureKeepsPartialCounts verifies a listing that
// fails mid-pass ends it with the error and the counts gathered so far.
func TestRunBulkRewrite_ListingFailureKeepsPartialCounts(t *testing.T) {
	t.Parallel()
	set := newShrinkingSet(pagingRows)
	set.failAfter = 1

	res, err := pagingOp(0).run(context.Background(), pagingEnv(t, set, leavingRewrite(set)), nil)
	if err == nil {
		t.Fatal("bulk rewrite succeeded over a failing listing")
	}
	if res.Objects == 0 || res.Objects >= pagingRows {
		t.Errorf("counts = %+v, want the first page's objects", res)
	}
}

// TestRunBulkRewrite_CancelStopsBeforeTheNextObject verifies a cancelled pass
// stops before handing the rewriter another object and reports the cancel.
func TestRunBulkRewrite_CancelStopsBeforeTheNextObject(t *testing.T) {
	t.Parallel()
	set := newShrinkingSet(3)
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	rewrite := rewriterFunc(func(context.Context, *writepath.RewriteRequest) []error {
		calls++
		cancel()
		return []error{nil}
	})

	res, err := pagingOp(0).run(ctx, pagingEnv(t, set, rewrite), nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancel", err)
	}
	if calls != 1 || res.Objects != 1 {
		t.Errorf("rewriter called %d times with counts %+v, want one object", calls, res)
	}
}

// TestRunBulkRewrite_ClassifiesEachCopyOutcome drives one copy through each
// answer a rewrite can give and checks where the pass tallies it and which
// counters move. A usage or target refusal is a skip, a copy a client replaced
// is changed, and anything else is a failure.
func TestRunBulkRewrite_ClassifiesEachCopyOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		err          error
		want         BulkRewriteResult
		label        string
		usageDecline float64
		copyChanged  float64
	}{
		{"placed", nil, BulkRewriteResult{Objects: 1, Succeeded: 1, Total: 1}, "success", 0, 0},
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
			op := pagingOp(0)
			op.opName = "outcome-test-" + tc.name

			res, err := op.run(context.Background(), pagingEnv(t, set, answering(tc.err)), nil)
			if err != nil {
				t.Fatalf("bulk rewrite: %v", err)
			}
			if res != tc.want {
				t.Errorf("counts = %+v, want %+v", res, tc.want)
			}
			assertOutcomeCounters(t, op, tc.label, tc.usageDecline, tc.copyChanged)
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
	op := pagingOp(0)
	op.onDeclined = func(_ context.Context, loc *core.ObjectLocation, encodedSize int64) {
		gotKey, gotSize = loc.ObjectKey, encodedSize
	}

	res, err := op.run(context.Background(), pagingEnv(t, set, answering(fmt.Errorf("reform: %w", &storedform.Declined{EncodedSize: 777}))), nil)
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
	if _, err := pagingOp(0).run(context.Background(), pagingEnv(t, set, answering(core.ErrCopyChanged)), nil); err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}
	if got := raced.Load(); got != 1 {
		t.Errorf("ConversionRaced events = %d, want 1", got)
	}
}

// TestRunBulkRewrite_RewritesInTheFormThePassWants checks each object is
// handed to the rewriter as the copies listed, with the pass's layers and the
// form it wants, and that the copy placed carries it.
func TestRunBulkRewrite_RewritesInTheFormThePassWants(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t)
	payload := []byte("hello world")
	be := &fakeBackend{payload: payload}
	rewriter := &fakeRewriter{backends: map[string]s3be.ObjectBackend{"backend-a": be}}
	set := newShrinkingSet(1)
	set.rows[0].ContentHash = "hash"
	set.rows[0].SizeBytes = int64(len(payload))
	env := bulkRewriteEnv{
		log:      slog.New(slog.DiscardHandler),
		store:    set,
		rewriter: rewriter,
		locker:   testx.Locker{},
		layers:   &storedform.Layers{Encryptor: enc},
	}
	op := pagingOp(0)
	op.layer, op.add = core.StoredEncrypted, true

	if _, err := op.run(context.Background(), env, nil); err != nil {
		t.Fatalf("bulk rewrite: %v", err)
	}
	reqs := rewriter.requests()
	if len(reqs) != 1 || len(reqs[0].Copies) != 1 || reqs[0].Copies[0].BackendName != "backend-a" {
		t.Fatalf("requests = %+v, want one of the listed copy", reqs)
	}
	if reqs[0].Layers != env.layers || !reqs[0].Want.Has(core.StoredEncrypted) {
		t.Errorf("request layers %p want %v, want the pass's layers and the copy encrypted", reqs[0].Layers, reqs[0].Want)
	}
	placed := rewriter.placements()
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
