// -------------------------------------------------------------------------------
// Batch Walk - The Page Loop Every Paged Pass Runs
//
// Author: Alex Freidah
//
// A Pager lists work a page at a time and hands each page to a visit function
// until the listing runs out, the visitor stops it, the context is cancelled, or
// something fails. It runs in one of two modes. With a cursor it lists past the
// last row of each page, so a row that fails is passed over and the next pass
// retries it once. Without one it re-lists the head of a listing whose rows
// leave as they are processed, such as a claimed queue, and ends the walk when a
// page completes nothing, since the same rows would come back forever.
// -------------------------------------------------------------------------------

package batch

import (
	"context"
	"iter"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Step is what visiting one page reports back: how many of its rows the page
// completed, and whether the walk should stop after it.
type Step struct {
	Progress int
	Stop     bool
}

// Stop is why a walk ended.
type Stop int

// Exhausted and the other reasons a walk ends.
const (
	Exhausted  Stop = iota // the listing ran out
	Stopped                // the visitor or BeforePage ended the walk
	NoProgress             // a head-mode page completed nothing
	Cancelled              // the context was cancelled between pages
	Errored                // listing, visiting or BeforePage failed
)

// Pager describes a paged listing. PageSize is read before each page, so a
// capped pass can ask for less on its last one. CursorOf selects the mode: set,
// each page starts after the previous page's last row; nil, each page re-lists
// the head. BeforePage is optional and runs before every page; false ends the
// walk.
type Pager[T, C any] struct {
	PageSize   func() int
	List       func(ctx context.Context, limit int, after C) ([]T, error)
	CursorOf   func(T) C
	BeforePage func(ctx context.Context) (bool, error)
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// FixedPage is the PageSize for a pass that always asks for n rows.
func FixedPage(n int) func() int {
	return func() int { return n }
}

// String names the stop reason for logs.
func (s Stop) String() string {
	switch s {
	case Exhausted:
		return "exhausted"
	case Stopped:
		return "stopped"
	case NoProgress:
		return "no_progress"
	case Cancelled:
		return "cancelled"
	default:
		return "errored"
	}
}

// Walk lists pages and hands each to visit until the walk ends, and reports
// why. A page shorter than asked for is the last one. The error is the failure
// when the walk ended Errored and the context's error when it ended Cancelled,
// so a cancelled pass reports the same way whether the cancel landed between
// pages or inside visit.
func (p Pager[T, C]) Walk(ctx context.Context, visit func(ctx context.Context, rows []T) (Step, error)) (Stop, error) {
	var after C
	for {
		if ok, stop, err := p.proceed(ctx); !ok {
			return stop, err
		}
		size := p.PageSize()
		rows, err := p.List(ctx, size, after)
		if err != nil {
			return Errored, err
		}
		if len(rows) == 0 {
			return Exhausted, nil
		}
		step, err := visit(ctx, rows)
		if err != nil {
			return Errored, err
		}
		if stop, done := p.ended(step, len(rows), size); done {
			return stop, nil
		}
		if p.CursorOf != nil {
			after = p.CursorOf(rows[len(rows)-1])
		}
	}
}

// proceed reports whether the walk may list another page, and when it may not,
// why and with what error.
func (p Pager[T, C]) proceed(ctx context.Context) (bool, Stop, error) {
	if err := ctx.Err(); err != nil {
		return false, Cancelled, err
	}
	if p.BeforePage == nil {
		return true, 0, nil
	}
	ok, err := p.BeforePage(ctx)
	switch {
	case err != nil:
		return false, Errored, err
	case !ok:
		return false, Stopped, nil
	default:
		return true, 0, nil
	}
}

// ended reports whether a visited page ends the walk, and why. The visitor's
// stop comes first, then a head-mode page that completed nothing, then a page
// shorter than asked for.
func (p Pager[T, C]) ended(step Step, got, asked int) (Stop, bool) {
	switch {
	case step.Stop:
		return Stopped, true
	case p.CursorOf == nil && step.Progress == 0:
		return NoProgress, true
	case got < asked:
		return Exhausted, true
	default:
		return 0, false
	}
}

// Rows yields every row the pager lists, one at a time, for a caller that
// consumes rows rather than pages, such as a merge pulling from two sorted
// streams. The walk ends when the consumer stops ranging. A walk that ends
// Errored or Cancelled yields its error last, with a zero row.
func (p Pager[T, C]) Rows(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		_, err := p.Walk(ctx, func(_ context.Context, rows []T) (Step, error) {
			for _, row := range rows {
				if !yield(row, nil) {
					return Step{Stop: true}, nil
				}
			}
			return Step{Progress: len(rows)}, nil
		})
		if err != nil {
			var zero T
			yield(zero, err)
		}
	}
}
