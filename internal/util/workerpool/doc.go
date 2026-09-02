// Package workerpool provides generic bounded-concurrency worker
// pool functions for parallel processing of work items. Context
// cancellation stops dispatching new work; in-flight items run to
// completion.
package workerpool
