// -------------------------------------------------------------------------------
// Admin API - Shared Cleanup DLQ DTOs
//
// Author: Alex Freidah
//
// Wire types for the cleanup dead-letter-queue endpoints shared by the handler
// and adminctl. Kept in the leaf adminapi package so the server and its
// out-of-process client depend on one definition and the JSON shape cannot
// drift.
// -------------------------------------------------------------------------------

package adminapi

import "time"

// CleanupDLQResponse is the dead-letter listing: the total depth plus a page of
// rows, newest graduation first.
type CleanupDLQResponse struct {
	Depth int64            `json:"depth"`
	Items []CleanupDLQItem `json:"items"`
}

// CleanupDLQItem is one dead-lettered cleanup: an object whose backend delete
// never succeeded within the retry budget and now needs operator attention.
type CleanupDLQItem struct {
	Backend       string    `json:"backend"`
	ObjectKey     string    `json:"object_key"`
	Reason        string    `json:"reason"`
	SizeBytes     int64     `json:"size_bytes"`
	Attempts      int32     `json:"attempts"`
	FirstEnqueued time.Time `json:"first_enqueued_at"`
	MovedAt       time.Time `json:"moved_at"`
	LastError     string    `json:"last_error,omitempty"`
}

// CleanupDLQRequeueResponse reports how many rows a requeue moved back into the
// cleanup queue, and the backend it was scoped to (empty means all backends).
type CleanupDLQRequeueResponse struct {
	Backend  string `json:"backend,omitempty"`
	Requeued int64  `json:"requeued"`
}
