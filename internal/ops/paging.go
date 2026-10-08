// -------------------------------------------------------------------------------
// Ops - Cursor Paging
//
// Author: Alex Freidah
//
// The page loop every fleet-wide pass runs. Each pass works through a listing
// whose rows leave the listing as they are processed, so paging is by cursor:
// an offset would skip the rows that move up to fill the gap and end the pass
// early while reporting success.
// -------------------------------------------------------------------------------

package ops

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// pageLister lists up to limit rows after the cursor, in cursor order.
type pageLister[T any] func(ctx context.Context, limit int, after core.Cursor) ([]T, error)

// walkPages lists rows a page at a time and hands each page to visit, until a
// page comes back shorter than asked, visit asks to stop, or either returns an
// error. pageSize is read before each page so a capped pass can shrink its last
// one. Reports whether visit stopped the walk.
func walkPages[T any](ctx context.Context, pageSize func() int, list pageLister[T], cursorOf func(T) core.Cursor,
	visit func(ctx context.Context, rows []T) (stop bool, err error)) (bool, error) {
	var after core.Cursor
	for {
		size := pageSize()
		rows, err := list(ctx, size, after)
		if err != nil {
			return false, err
		}
		if len(rows) == 0 {
			return false, nil
		}
		if stop, err := visit(ctx, rows); stop || err != nil {
			return stop, err
		}
		if len(rows) < size {
			return false, nil
		}
		after = cursorOf(rows[len(rows)-1])
	}
}

// fixedPage is the pageSize for a pass that always asks for the same amount.
func fixedPage(n int) func() int {
	return func() int { return n }
}
