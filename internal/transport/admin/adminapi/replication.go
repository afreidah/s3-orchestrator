// -------------------------------------------------------------------------------
// Admin API - Shared Replication DTOs
//
// Author: Alex Freidah
//
// Wire types for the replication endpoints shared by the handler and its
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

// ReplicationOutcome is the part every replication endpoint reports the same
// way: the terminal status, and why the pass did nothing when it was skipped.
// Status is "ok" when the endpoint acted and "skipped" when replication is
// unconfigured or the factor is 1; Reason accompanies "skipped" only. Embedded
// by the responses below so all three speak one vocabulary.
type ReplicationOutcome struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// ReplicateResponse reports a one-shot replication pass: how many copies were
// created to bring objects up to the configured factor.
type ReplicateResponse struct {
	ReplicationOutcome
	CopiesCreated int `json:"copies_created"`
}

// OverReplicationCleanResponse reports an over-replication cleanup pass: how
// many surplus copies were removed.
type OverReplicationCleanResponse struct {
	ReplicationOutcome
	CopiesRemoved int `json:"copies_removed"`
}

// OverReplicationStatusResponse is the over-replication backlog: the
// configured factor and the count of objects holding surplus copies. Both are
// zero when replication is not configured, which Status reports as "skipped"
// so the field means the same thing here as on the endpoints that act.
type OverReplicationStatusResponse struct {
	ReplicationOutcome
	Factor  int   `json:"factor"`
	Pending int64 `json:"pending"`
}
