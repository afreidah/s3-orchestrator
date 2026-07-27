// -------------------------------------------------------------------------------
// Admin API - Shared Backend-Management DTOs
//
// Author: Alex Freidah
//
// Wire types for the backend-management endpoints shared by the handler and
// adminctl. Kept in the leaf adminapi package so the server and its clients
// depend on one definition and the JSON shape cannot drift.
// -------------------------------------------------------------------------------

package adminapi

// RemoveBackendPreview is the confirmation payload returned by the purge-preview
// phase of DELETE /admin/api/backends/{name}: what a --purge would destroy and
// the token required to execute it.
type RemoveBackendPreview struct {
	Status       string `json:"status"`
	Backend      string `json:"backend"`
	ObjectCount  int64  `json:"object_count"`
	TotalBytes   int64  `json:"total_bytes"`
	ConfirmToken string `json:"confirm_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// BackendOperationResponse acknowledges a backend-management mutation: which
// backend was acted on, and what happened to it. Status is a human-readable
// outcome ("drain started", "drain cancelled", "backend removed", "backend
// purged"), matching the vocabulary RemoveBackendPreview already publishes for
// this endpoint family rather than the ok/skipped tokens the worker-trigger
// endpoints use.
type BackendOperationResponse struct {
	Status  string `json:"status"`
	Backend string `json:"backend"`
}

// DrainProgressResponse is a snapshot of an in-flight drain. Active is false
// when no drain is running for the backend, in which case the counters are
// zero. Error carries the failure that stopped a drain, when one did.
type DrainProgressResponse struct {
	Active           bool   `json:"active"`
	ObjectsRemaining int64  `json:"objects_remaining"`
	BytesRemaining   int64  `json:"bytes_remaining"`
	ObjectsMoved     int64  `json:"objects_moved"`
	Error            string `json:"error,omitempty"`
}
