// -------------------------------------------------------------------------------
// Batch Walk Tests
//
// Author: Alex Freidah
//
// Covers both modes: a cursor walk advancing past each page, and a head walk
// over a listing that shrinks as rows are processed, which must end when a page
// completes nothing. Also covers every other way a walk ends.
// -------------------------------------------------------------------------------

package batch

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// cursorPager pages over rows in order, listing past the cursor, and records
// each cursor it was asked from.
func cursorPager(rows []string, size int, cursors *[]string) Pager[string, string] {
	return Pager[string, string]{
		PageSize: FixedPage(size),
		List: func(_ context.Context, limit int, after string) ([]string, error) {
			*cursors = append(*cursors, after)
			var out []string
			for _, r := range rows {
				if r > after && len(out) < limit {
					out = append(out, r)
				}
			}
			return out, nil
		},
		CursorOf: func(r string) string { return r },
	}
}

// headPager pages over the front of queue, which the visitor shrinks.
func headPager(queue *[]int, size int) Pager[int, struct{}] {
	return Pager[int, struct{}]{
		PageSize: FixedPage(size),
		List: func(_ context.Context, limit int, _ struct{}) ([]int, error) {
			return slices.Clone((*queue)[:min(limit, len(*queue))]), nil
		},
	}
}

// -------------------------------------------------------------------------
// CURSOR MODE
// -------------------------------------------------------------------------

// TestWalk_CursorAdvancesPastEachPage verifies each page is listed after the
// last row of the one before, and a short page ends the walk.
func TestWalk_CursorAdvancesPastEachPage(t *testing.T) {
	t.Parallel()
	rows := []string{"a", "b", "c", "d", "e"}
	var cursors, seen []string
	stop, err := cursorPager(rows, 2, &cursors).Walk(context.Background(), func(_ context.Context, page []string) (Step, error) {
		seen = append(seen, page...)
		return Step{}, nil
	})
	if err != nil || stop != Exhausted {
		t.Fatalf("Walk = (%v, %v), want (exhausted, nil)", stop, err)
	}
	if !slices.Equal(seen, rows) {
		t.Errorf("seen = %v, want %v", seen, rows)
	}
	if want := []string{"", "b", "d"}; !slices.Equal(cursors, want) {
		t.Errorf("cursors = %v, want %v", cursors, want)
	}
}

// TestWalk_CursorPassesOverFailedRows verifies a cursor walk carries on past a
// page that completed nothing: the failed rows are behind the cursor, so the
// next page holds new rows.
func TestWalk_CursorPassesOverFailedRows(t *testing.T) {
	t.Parallel()
	var cursors []string
	pages := 0
	stop, err := cursorPager([]string{"a", "b", "c", "d"}, 2, &cursors).Walk(context.Background(), func(context.Context, []string) (Step, error) {
		pages++
		return Step{Progress: 0}, nil
	})
	if err != nil || stop != Exhausted || pages != 2 {
		t.Errorf("Walk = (%v, %v) after %d pages, want (exhausted, nil) after 2", stop, err, pages)
	}
}

// -------------------------------------------------------------------------
// HEAD MODE
// -------------------------------------------------------------------------

// TestWalk_HeadDrainsAShrinkingListing verifies a head walk re-lists the front
// until the listing runs out, as each processed row leaves it.
func TestWalk_HeadDrainsAShrinkingListing(t *testing.T) {
	t.Parallel()
	queue := []int{1, 2, 3, 4, 5}
	var seen []int
	stop, err := headPager(&queue, 2).Walk(context.Background(), func(_ context.Context, page []int) (Step, error) {
		seen = append(seen, page...)
		queue = queue[len(page):]
		return Step{Progress: len(page)}, nil
	})
	if err != nil || stop != Exhausted {
		t.Fatalf("Walk = (%v, %v), want (exhausted, nil)", stop, err)
	}
	if !slices.Equal(seen, []int{1, 2, 3, 4, 5}) {
		t.Errorf("seen = %v, want every row once", seen)
	}
}

// TestWalk_HeadStopsWhenAPageCompletesNothing verifies a head walk ends with
// NoProgress instead of re-listing the same stuck rows forever, even on a
// short page.
func TestWalk_HeadStopsWhenAPageCompletesNothing(t *testing.T) {
	t.Parallel()
	for _, size := range []int{2, 5} {
		queue := []int{1, 2, 3}
		lists := 0
		p := headPager(&queue, size)
		list := p.List
		p.List = func(ctx context.Context, limit int, after struct{}) ([]int, error) {
			lists++
			return list(ctx, limit, after)
		}
		stop, err := p.Walk(context.Background(), func(context.Context, []int) (Step, error) {
			return Step{}, nil
		})
		if err != nil || stop != NoProgress || lists != 1 {
			t.Errorf("size %d: Walk = (%v, %v) after %d lists, want (no_progress, nil) after 1", size, stop, err, lists)
		}
	}
}

// -------------------------------------------------------------------------
// OTHER ENDINGS
// -------------------------------------------------------------------------

// TestWalk_StopsWhenVisitAsks verifies a visit asking to stop ends the walk
// without listing another page.
func TestWalk_StopsWhenVisitAsks(t *testing.T) {
	t.Parallel()
	var cursors []string
	stop, err := cursorPager([]string{"a", "b", "c"}, 1, &cursors).Walk(context.Background(), func(context.Context, []string) (Step, error) {
		return Step{Progress: 1, Stop: true}, nil
	})
	if err != nil || stop != Stopped || len(cursors) != 1 {
		t.Errorf("Walk = (%v, %v) after %d lists, want (stopped, nil) after 1", stop, err, len(cursors))
	}
}

// TestWalk_BeforePageEndsTheWalk verifies BeforePage runs ahead of every page,
// and that false or an error ends the walk before the page is listed.
func TestWalk_BeforePageEndsTheWalk(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom")
	for _, tc := range []struct {
		name     string
		ok       bool
		err      error
		wantStop Stop
	}{
		{"declined", false, nil, Stopped},
		{"failed", false, wantErr, Errored},
	} {
		var cursors []string
		p := cursorPager([]string{"a", "b"}, 1, &cursors)
		checks := 0
		p.BeforePage = func(context.Context) (bool, error) {
			checks++
			if checks == 2 {
				return tc.ok, tc.err
			}
			return true, nil
		}
		stop, err := p.Walk(context.Background(), func(context.Context, []string) (Step, error) {
			return Step{Progress: 1}, nil
		})
		if stop != tc.wantStop || !errors.Is(err, tc.err) || len(cursors) != 1 {
			t.Errorf("%s: Walk = (%v, %v) after %d lists, want (%v, %v) after 1", tc.name, stop, err, len(cursors), tc.wantStop, tc.err)
		}
	}
}

// TestWalk_CancelledBetweenPages verifies a cancelled context ends the walk
// before the next page is listed, with the context's error.
func TestWalk_CancelledBetweenPages(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var cursors []string
	stop, err := cursorPager([]string{"a", "b", "c"}, 1, &cursors).Walk(ctx, func(context.Context, []string) (Step, error) {
		cancel()
		return Step{Progress: 1}, nil
	})
	if !errors.Is(err, context.Canceled) || stop != Cancelled || len(cursors) != 1 {
		t.Errorf("Walk = (%v, %v) after %d lists, want (cancelled, context.Canceled) after 1", stop, err, len(cursors))
	}
}

// TestWalk_ReturnsListAndVisitErrors verifies a failure on either side ends
// the walk Errored with that error.
func TestWalk_ReturnsListAndVisitErrors(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom")
	failingList := Pager[int, struct{}]{
		PageSize: FixedPage(1),
		List:     func(context.Context, int, struct{}) ([]int, error) { return nil, wantErr },
	}
	queue := []int{1}
	okList := headPager(&queue, 1)

	if stop, err := failingList.Walk(context.Background(), func(context.Context, []int) (Step, error) {
		return Step{}, nil
	}); stop != Errored || !errors.Is(err, wantErr) {
		t.Errorf("list failure: Walk = (%v, %v), want (errored, %v)", stop, err, wantErr)
	}
	if stop, err := okList.Walk(context.Background(), func(context.Context, []int) (Step, error) {
		return Step{}, wantErr
	}); stop != Errored || !errors.Is(err, wantErr) {
		t.Errorf("visit failure: Walk = (%v, %v), want (errored, %v)", stop, err, wantErr)
	}
}

// TestRows_YieldsEveryRowAcrossPages verifies the row iterator walks every
// page and stops listing as soon as the consumer stops ranging.
func TestRows_YieldsEveryRowAcrossPages(t *testing.T) {
	t.Parallel()
	var cursors []string
	var seen []string
	for row, err := range cursorPager([]string{"a", "b", "c", "d", "e"}, 2, &cursors).Rows(context.Background()) {
		if err != nil {
			t.Fatalf("Rows: %v", err)
		}
		seen = append(seen, row)
	}
	if !slices.Equal(seen, []string{"a", "b", "c", "d", "e"}) {
		t.Errorf("seen = %v, want every row in order", seen)
	}

	cursors = nil
	for row := range cursorPager([]string{"a", "b", "c", "d", "e"}, 2, &cursors).Rows(context.Background()) {
		if row == "b" {
			break
		}
	}
	if len(cursors) != 1 {
		t.Errorf("listed %d pages after stopping on the first, want 1", len(cursors))
	}
}

// TestRows_YieldsTheListError verifies a failed listing reaches the consumer
// as the last value.
func TestRows_YieldsTheListError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom")
	p := Pager[int, struct{}]{
		PageSize: FixedPage(1),
		List:     func(context.Context, int, struct{}) ([]int, error) { return nil, wantErr },
	}
	var got error
	for _, err := range p.Rows(context.Background()) {
		got = err
	}
	if !errors.Is(got, wantErr) {
		t.Errorf("last error = %v, want %v", got, wantErr)
	}
}

// TestStop_String verifies every reason has a distinct log name.
func TestStop_String(t *testing.T) {
	t.Parallel()
	want := map[Stop]string{
		Exhausted: "exhausted", Stopped: "stopped", NoProgress: "no_progress",
		Cancelled: "cancelled", Errored: "errored",
	}
	for s, name := range want {
		if got := s.String(); got != name {
			t.Errorf("Stop(%d).String() = %q, want %q", s, got, name)
		}
	}
}
