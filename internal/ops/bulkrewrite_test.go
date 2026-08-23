// -------------------------------------------------------------------------------
// Ops - Bulk Rewrite Paging Tests
//
// Author: Alex Freidah
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
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/mock/gomock"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// pagingRows is how many rows these tests seed: enough to span several batches,
// since a paging fault is invisible inside a single page.
const pagingRows = 250

// shrinkingSet is a listing whose rows leave it as they are processed, which is
// what all four real bulk-rewrite listings do. Rows are held in key order and
// served from a cursor, exactly as the SQL does.
type shrinkingSet struct {
	mu      sync.Mutex
	rows    []core.RewritableLocation
	served  int
	visited map[string]int
}

// newShrinkingSet seeds n rows under keys that sort in the order the listings
// return them.
func newShrinkingSet(n int) *shrinkingSet {
	s := &shrinkingSet{visited: map[string]int{}}
	for i := range n {
		s.rows = append(s.rows, core.RewritableLocation{
			ObjectKey:   fmt.Sprintf("obj-%04d", i),
			BackendName: "backend-a",
			SizeBytes:   1024,
		})
	}
	return s
}

// page returns the rows after the cursor, up to limit. served bounds the total
// handed out so a driver that never advances its cursor fails the test instead
// of spinning forever.
func (s *shrinkingSet) page(_ context.Context, limit int, after core.Cursor) ([]core.RewritableLocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []core.RewritableLocation
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
func slicesDelete(rows []core.RewritableLocation, key string) []core.RewritableLocation {
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

// pagingEnv builds the collaborators the driver needs, over a backend that
// serves and accepts anything.
func pagingEnv(t *testing.T) bulkRewriteEnv {
	t.Helper()
	ctrl := gomock.NewController(t)

	runtime := opstest.NewMockRuntimeOps(ctrl)
	runtime.EXPECT().GetBackend(gomock.Any()).Return(&fakeBackend{payload: []byte("payload")}, nil).AnyTimes()
	backendOps := opstest.NewMockBackendOps(ctrl)
	backendOps.EXPECT().RecordUsage(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	return bulkRewriteEnv{
		log:        slog.New(slog.DiscardHandler),
		runtime:    runtime,
		backendOps: backendOps,
	}
}

// pagingCounter is a throwaway counter so these tests do not move the process
// metrics every other test in the package reads.
func pagingCounter() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bulk_rewrite_paging_test"}, []string{"status"})
}

// TestRunBulkRewrite_ProcessesEveryRowAsTheSetShrinks is the regression test for
// the paging fault. Every row here succeeds and therefore leaves the listing,
// which is the decompress-existing case: with an offset walk the pass stepped
// over a page for every page it completed and stopped early, reporting a clean
// run over roughly half the fleet.
func TestRunBulkRewrite_ProcessesEveryRowAsTheSetShrinks(t *testing.T) {
	t.Parallel()
	set := newShrinkingSet(pagingRows)

	res, err := runBulkRewrite(pagingEnv(t), context.Background(), nil, bulkRewriteOp[*rewriteRow]{
		opName:      "paging-test",
		resultLabel: "rewritten",
		counter:     pagingCounter(),
		listFn:      rewriteListFn(set.page),
		rewrite: func(_ context.Context, _ *s3be.GetObjectResult, loc *rewriteRow) (rewritten, error) {
			key := loc.rewriteKey()
			set.visit(key)
			return rewritten{
				body: strings.NewReader("rewritten"),
				size: int64(len("rewritten")),
				commit: func() error {
					set.remove(key)
					return nil
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("runBulkRewrite: %v", err)
	}

	if res.Succeeded != pagingRows || res.Total != pagingRows {
		t.Errorf("counts = %+v, want %d succeeded out of %d", res, pagingRows, pagingRows)
	}
	set.assertVisitedAll(t, pagingRows)
}

// TestRunBulkRewrite_TerminatesWhenRowsStay covers the other half of the
// contract, and rules out the naive repair. A pass that simply re-queried from
// the start each time would walk a shrinking set correctly and never finish
// this one: a declined row stays in the listing, so it would be served forever.
// Compress-existing declines most of a fleet of media, so this is the ordinary
// case rather than the exotic one.
func TestRunBulkRewrite_TerminatesWhenRowsStay(t *testing.T) {
	t.Parallel()
	set := newShrinkingSet(pagingRows)

	res, err := runBulkRewrite(pagingEnv(t), context.Background(), nil, bulkRewriteOp[*rewriteRow]{
		opName:      "paging-test",
		resultLabel: "rewritten",
		counter:     pagingCounter(),
		listFn:      rewriteListFn(set.page),
		declines: func(loc *rewriteRow) bool {
			set.visit(loc.rewriteKey())
			return true
		},
		rewrite: func(context.Context, *s3be.GetObjectResult, *rewriteRow) (rewritten, error) {
			t.Error("a declined row must not be downloaded or rewritten")
			return rewritten{}, nil
		},
	})
	if err != nil {
		t.Fatalf("runBulkRewrite: %v", err)
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

	res, err := runBulkRewrite(pagingEnv(t), context.Background(), nil, bulkRewriteOp[*rewriteRow]{
		opName:      "paging-test",
		resultLabel: "rewritten",
		counter:     pagingCounter(),
		listFn:      rewriteListFn(set.page),
		declines: func(loc *rewriteRow) bool {
			if !declined(loc.rewriteKey()) {
				return false
			}
			set.visit(loc.rewriteKey())
			return true
		},
		rewrite: func(_ context.Context, _ *s3be.GetObjectResult, loc *rewriteRow) (rewritten, error) {
			key := loc.rewriteKey()
			set.visit(key)
			return rewritten{
				body: strings.NewReader("rewritten"),
				size: int64(len("rewritten")),
				commit: func() error {
					set.remove(key)
					return nil
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("runBulkRewrite: %v", err)
	}

	if res.Total != pagingRows || res.Succeeded+res.Skipped != pagingRows {
		t.Errorf("counts = %+v, want %d rows all accounted for", res, pagingRows)
	}
	if res.Skipped == 0 || res.Succeeded == 0 {
		t.Errorf("counts = %+v, want both outcomes represented", res)
	}
	set.assertVisitedAll(t, pagingRows)
}
