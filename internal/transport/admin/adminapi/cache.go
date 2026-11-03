// -------------------------------------------------------------------------------
// Admin API - Shared Object Data Cache DTOs
//
// Author: Alex Freidah
//
// Wire types for the /admin/api/cache endpoints shared by the handler and its
// clients. Kept in the leaf adminapi package so the server and its
// out-of-process client depend on one definition and the JSON shape cannot
// drift.
// -------------------------------------------------------------------------------

package adminapi

// CacheStatsResponse is the object data cache's current utilization. Mirrors
// the s3o_cache_* gauges for operators without Prometheus access.
type CacheStatsResponse struct {
	Entries   int   `json:"entries"`
	SizeBytes int64 `json:"size_bytes"`
	MaxBytes  int64 `json:"max_bytes"`
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
}

// CacheInvalidateResponse is the outcome of dropping cache entries in bulk:
// either a full flush (Status "flushed") or a prefix sweep (Status
// "invalidated", Prefix set). EntriesDropped counts the entries removed under
// one name for both, so a caller does not need to know which route produced
// the body.
type CacheInvalidateResponse struct {
	Status         string `json:"status"`
	Prefix         string `json:"prefix,omitempty"`
	EntriesDropped int    `json:"entries_dropped"`
}

// CacheInvalidateKeyResponse is the outcome of dropping one key. It carries no
// count: the cache treats an unknown key as a no-op and reports nothing back,
// so a count here could only ever be a guess.
type CacheInvalidateKeyResponse struct {
	Status string `json:"status"`
	Key    string `json:"key"`
}
