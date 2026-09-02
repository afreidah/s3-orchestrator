// -------------------------------------------------------------------------------
// Object Cache - Interface and Types
//
// Author: Alex Freidah
//
// Defines the ObjectCache interface for caching object data to reduce backend
// API calls and egress. Implementations must be safe for concurrent use.
// -------------------------------------------------------------------------------

package cache

import "time"

// -------------------------------------------------------------------------
// INTERFACE
// -------------------------------------------------------------------------

// ObjectCache caches object data to avoid repeated backend fetches.
// Implementations must be safe for concurrent use by multiple goroutines.
//
// The interface separates admission decision from buffering so callers
// can refuse to read oversized payloads into memory in the first place:
// check Admit(size), and only buffer + PutBytes when admitted.
//
// PutBytes is best-effort. A caller that admitted an entry may still find the
// cache unable to hold it once larger entries have taken the capacity, and that
// case is a silent no-op rather than an error, because nothing about the read
// it came from has gone wrong.
//
// An empty prefix passed to InvalidatePrefix matches every entry. Callers that
// mean to empty the cache should call Clear instead, so the metric records what
// the operator actually asked for.
type ObjectCache interface {
	Get(key string) (*Entry, bool)
	Admit(size int64) bool // O(1) size check, before any buffering
	PutBytes(key string, data []byte, meta EntryMeta)
	Invalidate(key string)
	InvalidatePrefix(prefix string) int // returns entries dropped
	Clear() int                         // returns entries dropped
	Stats() Stats
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Entry holds a cached object's data and metadata. TagCount rides along
// because a cache hit answers the whole GET without consulting the metadata
// store, and the tagging-count header would otherwise be missing from exactly
// the responses the cache serves. Tag writes invalidate the entry.
type Entry struct {
	Data         []byte
	ContentType  string
	ETag         string
	LastModified time.Time
	Metadata     map[string]string
	TagCount     int
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
	ContentType  string
	ETag         string
	LastModified time.Time
	Metadata     map[string]string
	TagCount     int
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
