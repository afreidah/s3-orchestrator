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

import "io"

// -------------------------------------------------------------------------
// INTERFACE
// -------------------------------------------------------------------------

// ObjectCache caches object data to avoid repeated backend fetches.
// Implementations must be safe for concurrent use by multiple goroutines.
type ObjectCache interface {
	// Get returns the cached entry for the given key, or false if not cached.
	// The returned reader must be closed by the caller.
	Get(key string) (*Entry, bool)

	// Put stores an object in the cache. The data is read fully from r.
	// If the object exceeds the max object size or the cache is at capacity,
	// the entry may be rejected or older entries evicted.
	Put(key string, r io.Reader, meta EntryMeta) error

	// Invalidate removes a single key from the cache.
	Invalidate(key string)

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

// Stats reports current cache utilization.
type Stats struct {
	Entries  int
	SizeBytes int64
	MaxBytes int64
}
