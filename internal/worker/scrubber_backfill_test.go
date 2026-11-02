// -------------------------------------------------------------------------------
// Backfill Tests
//
// Author: Alex Freidah
//
// Drives the backfill's page walk over a backlog of unhashed copies that
// shrinks as each copy is hashed, the case offset paging gets wrong: every
// copy is hashed across pages, the object cap stops the run between pages,
// copies that cannot be decoded are stepped past rather than listed again,
// and the pause between pages both runs and honours cancellation.
// -------------------------------------------------------------------------------

package worker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// backlogScrubber builds a scrubber over a backlog of n unhashed copies whose
// bytes every read serves. undecodable names the copies stored in a form the
// scrubber has no codec for.
func backlogScrubber(t *testing.T, n int, undecodable func(key string) bool) (*Scrubber, *mockMetadataStore) {
	t.Helper()
	s, ops, _, be, ms := setupScrubber(t)
	for i := range n {
		row := core.ObjectLocation{ObjectKey: fmt.Sprintf("k-%03d", i), BackendName: "b1", SizeBytes: 5}
		if undecodable != nil && undecodable(row.ObjectKey) {
			row.CompressionAlgorithm = compression.Algorithm
			row.CompressionFormatVersion = compression.FormatVersion
		}
		ms.unhashed = append(ms.unhashed, row)
	}
	ops.EXPECT().GetBackend("b1").Return(be, nil).AnyTimes()
	ops.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), "").
		DoAndReturn(func(context.Context, string, backend.ObjectBackend, string, string) (*backend.GetObjectResult, context.CancelFunc, error) {
			return &backend.GetObjectResult{Body: io.NopCloser(strings.NewReader("hello")), Size: 5}, func() {}, nil
		}).AnyTimes()
	return s, ms
}

// TestBackfill_HashesEveryRowAcrossPages verifies a backlog several pages long
// is hashed whole, even though each hashed copy leaves the listing it was
// read from.
func TestBackfill_HashesEveryRowAcrossPages(t *testing.T) {
	t.Parallel()
	s, ms := backlogScrubber(t, 25, nil)
	res, err := s.Backfill(context.Background(), BackfillRequest{BatchSize: 10}, nil)
	if err != nil || res.Succeeded != 25 || !res.Done || len(ms.unhashed) != 0 {
		t.Errorf("res, err = %+v, %v with %d left; want all 25 hashed and the backlog drained", res, err, len(ms.unhashed))
	}
}

// TestBackfill_StopsAtObjectCap verifies the cap bounds one run, checked
// between pages so the page that crossed it completes, and that the run
// reports the backlog as not drained.
func TestBackfill_StopsAtObjectCap(t *testing.T) {
	t.Parallel()
	s, ms := backlogScrubber(t, 40, nil)
	res, err := s.Backfill(context.Background(), BackfillRequest{BatchSize: 10, MaxObjects: 25}, nil)
	if err != nil || res.Succeeded != 30 || len(ms.unhashed) != 10 || res.Done {
		t.Errorf("res, err = %+v, %v with %d left; want 30 hashed, 10 left and not done", res, err, len(ms.unhashed))
	}
}

// TestBackfill_StepsPastUndecodableCopies verifies copies that cannot be
// decoded are skipped and stay unhashed, and the cursor steps past them
// rather than listing them again.
func TestBackfill_StepsPastUndecodableCopies(t *testing.T) {
	t.Parallel()
	s, _ := backlogScrubber(t, 13, func(key string) bool { return key < "k-005" })
	res, err := s.Backfill(context.Background(), BackfillRequest{BatchSize: 10}, nil)
	if err != nil || res.Skipped != 5 || res.Succeeded != 8 || !res.Done {
		t.Errorf("res, err = %+v, %v; want 5 skipped, 8 hashed and the backlog drained", res, err)
	}
}

// TestBackfill_PausesBetweenPages verifies the pause runs between pages,
// which is what rate-limits backend reads on a large backlog.
func TestBackfill_PausesBetweenPages(t *testing.T) {
	t.Parallel()
	s, _ := backlogScrubber(t, 15, nil)
	start := time.Now()
	res, err := s.Backfill(context.Background(), BackfillRequest{BatchSize: 10, Pause: 20 * time.Millisecond}, nil)
	if err != nil || !res.Done {
		t.Fatalf("res, err = %+v, %v; want the backlog drained", res, err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("elapsed = %v, want at least the pause between pages", elapsed)
	}
}

// TestBackfill_StopsOnCancelledContext verifies an operator who disconnects
// mid-run stops the run during the pause rather than leaving it draining the
// backlog.
func TestBackfill_StopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	s, ms := backlogScrubber(t, 20, nil)
	ctx, cancel := context.WithCancel(context.Background())
	ended := 0
	observer := func(step progress.Step) {
		if step.Phase == progress.PhaseEnd {
			if ended++; ended == 10 {
				cancel()
			}
		}
	}
	res, err := s.Backfill(ctx, BackfillRequest{BatchSize: 10, Pause: time.Hour}, observer)
	if err != nil || res.Done || len(ms.unhashed) != 10 {
		t.Errorf("res, err = %+v, %v with %d left; want one page hashed and not done", res, err, len(ms.unhashed))
	}
}
