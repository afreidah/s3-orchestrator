// Package syncutil provides small concurrency primitives the rest of
// the codebase shares: AtomicConfig wraps atomic.Pointer[T] for
// hot-reloadable config, Hook holds an optional runtime callback, TTLCache
// implements a generic time-bounded cache with background eviction, Every
// is the one periodic loop, and Sleep and Receive are the waits that end with
// their context.
package syncutil
