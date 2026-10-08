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
// purged"), matching RemoveBackendPreview rather than the ok/skipped tokens of
// the worker-trigger endpoints.
type BackendOperationResponse struct {
	Status  string `json:"status"`
	Backend string `json:"backend"`
}

// DrainProgressResponse is a snapshot of a backend's drain. State is draining,
// drained, or failed, and empty when the backend has no drain record. Active is
// true only while the drain is in progress, and the remaining counts are read
// only then. Error carries the failure that stopped a drain, when one did.
type DrainProgressResponse struct {
	Active           bool   `json:"active"`
	State            string `json:"state,omitempty"`
	ObjectsRemaining int64  `json:"objects_remaining"`
	BytesRemaining   int64  `json:"bytes_remaining"`
	ObjectsMoved     int64  `json:"objects_moved"`
	Error            string `json:"error,omitempty"`
}
