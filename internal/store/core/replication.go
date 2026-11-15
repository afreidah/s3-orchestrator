// -------------------------------------------------------------------------------
// Core Replication Orchestration
//
// Author: Alex Freidah
//
// Types for replica management. A key off the replication factor is held in
// the replication queue until a drain brings it back: the ledger edit queues
// the keys its transaction left off the factor, and a rebuild queues every one
// the ledger holds. A replica is recorded by PlaceCopy, and surplus copies are
// removed by DeleteObjectLocation with keepAtLeast set.
// -------------------------------------------------------------------------------

package core

// ReplicationDirection says which side of the factor a queued key is on.
type ReplicationDirection int16

// A queued key is short of the factor or past it.
const (
	ReplicationShort ReplicationDirection = -1
	ReplicationOver  ReplicationDirection = 1
)

// directionOf returns the side of factor that count is on.
func directionOf(count, factor int) ReplicationDirection {
	if count < factor {
		return ReplicationShort
	}
	return ReplicationOver
}

// ReplicationBacklog counts the queued keys on each side of the factor.
type ReplicationBacklog struct {
	Under int64
	Over  int64
}

// QueuedReplication is a key the replication queue holds, as a drain read it.
// Live counts its copies outside the excluded backends plus its live intents,
// the copies it will have once every upload lands; Held counts every managed
// copy recorded. Version is what a settle or retry must match.
type QueuedReplication struct {
	Key       string
	Direction ReplicationDirection
	Attempts  int32
	Version   int64
	Live      int
	Held      int
}
