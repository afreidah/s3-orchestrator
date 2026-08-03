// -------------------------------------------------------------------------------
// Object Cache - Interface and Types
//
// Author: Alex Freidah
//
// Defines the ObjectCache interface for caching object data to reduce backend
// API calls and egress. Implementations must be safe for concurrent use.
// -------------------------------------------------------------------------------

// Package cache provides an object data cache layer that sits between the
// orchestrator and storage backends, reducing API calls and egress by serving
// repeated reads from local storage.
package cache

// -------------------------------------------------------------------------
// INTERFACE
// -------------------------------------------------------------------------

// ObjectCache caches object data to avoid repeated backend fetches.
// Implementations must be safe for concurrent use by multiple goroutines.
//
// The interface separates admission decision from buffering so callers
// can refuse to read oversized payloads into memory in the first place:
// check Admit(size), and only buffer + PutBytes when admitted.
type ObjectCache interface {
	// Get returns the cached entry for the given key, or false if not cached.
	Get(key string) (*Entry, bool)

	// Admit reports whether an entry of the given size would be accepted by
	// the cache (i.e., size <= max_object_size). O(1). Callers consult this
	// before buffering data; oversized streams stay unread on the wire.
	Admit(size int64) bool

	// PutBytes stores pre-buffered data for key. Callers must have called
	// Admit first to confirm size fits. Implementations may evict LRU
	// entries to make room. Best-effort: if the cache cannot store the
	// entry (e.g. capacity exhausted by larger entries), the call is a
	// silent no-op.
	PutBytes(key string, data []byte, meta EntryMeta)

	// Invalidate removes a single key from the cache.
	Invalidate(key string)

	// InvalidatePrefix removes every entry whose key starts with the given
	// prefix and returns the count of entries that were dropped. Empty
	// prefix matches every entry (equivalent to Clear) - callers that
	// want a clean flush should use Clear directly so the metric counter
	// reflects the operator's intent.
	InvalidatePrefix(prefix string) int

	// Clear removes every entry. Returns the number of entries that were
	// dropped. Used by the admin cache-flush endpoint to support cache-cold
	// performance characterisation.
	Clear() int

	// Stats returns current cache utilization statistics.
	Stats() Stats
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Entry holds a cached object's data and metadata.
type Entry struct {
	Data        []byte
	ContentType string
	ETag        string
	Metadata    map[string]string
}

// Size returns the approximate memory footprint of this entry in bytes.
func (e *Entry) Size() int64 {
	n := int64(len(e.Data))
	n += int64(len(e.ContentType))
	n += int64(len(e.ETag))
	for k, v := range e.Metadata {
		n += int64(len(k) + len(v))
	}
	return n
}

// EntryMeta holds the metadata to store alongside cached object data.
type EntryMeta struct {
	ContentType string
	ETag        string
	Metadata    map[string]string
}

// Stats reports current cache utilization. Hits and Misses are lifetime
// process totals, mirroring the s3o_cache_hits_total / s3o_cache_misses_total
// counters so a caller without Prometheus can still derive a hit rate.
type Stats struct {
	Entries   int
	SizeBytes int64
	MaxBytes  int64
	Hits      int64
	Misses    int64
}
