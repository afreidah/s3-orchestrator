// Package batch runs work in batches. A Pager lists the work a page at a time
// and walks it, by cursor or by re-listing the head of a shrinking queue, until
// the listing runs out or the walk is stopped. Runner takes one page of items
// through a per-item function with bounded concurrency and tallies the outcomes
// into a Summary, so every caller reports its work the same way and feeds the
// same outcome label to metrics.
package batch
