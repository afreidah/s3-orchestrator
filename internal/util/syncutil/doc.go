// Package syncutil provides small concurrency primitives the rest of
// the codebase shares: AtomicConfig wraps atomic.Pointer[T] for
// hot-reloadable config, and TTLCache implements a generic
// time-bounded cache with background eviction.
package syncutil
