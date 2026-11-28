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

// ReplicationFactors is how many copies replication keeps each key at: Factor
// for a virtual bucket without a factor of its own, and BucketFactors for the
// virtual buckets that set one. BucketFactors is omitted when none does.
type ReplicationFactors struct {
	Factor        int            `json:"factor"`
	BucketFactors map[string]int `json:"bucket_factors,omitempty"`
}

// ReplicationStatusResponse is a snapshot of the replication backlog: the
// running factors, whether they leave the workers anything to keep, the count
// of under-replicated objects (waiting to be copied up to their factor) and
// over-replicated objects (waiting for cleanup), and when the snapshot was
// computed. The counts are zero while Replicating is false.
type ReplicationStatusResponse struct {
	ReplicationFactors
	Replicating     bool      `json:"replicating"`
	UnderReplicated int64     `json:"under_replicated"`
	OverReplicated  int64     `json:"over_replicated"`
	ComputedAt      time.Time `json:"computed_at"`
}

// ReplicateResponse reports a one-shot replication pass: how many copies were
// created to bring objects up to their factor, and how many objects the pass
// could not bring up to it. Failed is omitted when the pass left nothing
// behind, so a clean run reads exactly as it did before. A pass is skipped
// when no key's factor leaves anything to replicate.
type ReplicateResponse struct {
	Outcome
	CopiesCreated int `json:"copies_created"`
	Failed        int `json:"failed,omitempty"`
}

// ReplicationRebuildResponse reports a rebuild of the replication queue from
// the ledger: the factors it counted against and how many keys it queued. A
// rebuild is skipped when no key's factor leaves anything to replicate, or
// while a replication pass is running.
type ReplicationRebuildResponse struct {
	Outcome
	ReplicationFactors
	Queued int64 `json:"queued"`
}

// OverReplicationCleanResponse reports an over-replication cleanup pass: how
// many surplus copies were removed, and how many objects still carry surplus
// the pass could not remove.
type OverReplicationCleanResponse struct {
	Outcome
	CopiesRemoved int `json:"copies_removed"`
	Failed        int `json:"failed,omitempty"`
}

// OverReplicationStatusResponse is the over-replication backlog: the running
// factors and the count of objects holding surplus copies. Both are zero when
// replication is not configured, which Status reports as "skipped" so the
// field means the same thing here as on the endpoints that act.
type OverReplicationStatusResponse struct {
	Outcome
	ReplicationFactors
	Pending int64 `json:"pending"`
}
