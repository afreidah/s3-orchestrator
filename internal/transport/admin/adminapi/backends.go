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
