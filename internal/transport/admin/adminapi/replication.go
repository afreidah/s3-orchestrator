// -------------------------------------------------------------------------------
// Admin API - Shared Replication DTOs
//
// Author: Alex Freidah
//
// Wire types for the replication-status endpoint shared by the handler and its
// clients (the TUI). Kept in the leaf adminapi package so the server and its
// clients depend on one definition and the JSON shape cannot drift.
// -------------------------------------------------------------------------------

package adminapi

import "time"

// ReplicationStatusResponse is a snapshot of the replication backlog: the
// configured factor, the count of under-replicated objects (waiting to be
// copied up to factor) and over-replicated objects (waiting for cleanup), and
// when the snapshot was computed. Factor <= 1 means replication is disabled.
type ReplicationStatusResponse struct {
	Factor          int       `json:"factor"`
	UnderReplicated int64     `json:"under_replicated"`
	OverReplicated  int64     `json:"over_replicated"`
	ComputedAt      time.Time `json:"computed_at"`
}
