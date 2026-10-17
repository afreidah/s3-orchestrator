// -------------------------------------------------------------------------------
// Core Replication Orchestration
//
// Author: Alex Freidah
//
// Types for replica management. A replica is recorded by PlaceCopy, and
// surplus copies are removed by DeleteObjectLocation with keepAtLeast set.
// -------------------------------------------------------------------------------

package core

// ReplicationBacklog counts the keys whose copy count is off the target
// factor. Under counts in-flight intents as copies, as the replicator's scan
// does; Over counts recorded copies only, as the cleaner's scan does.
type ReplicationBacklog struct {
	Under int64
	Over  int64
}
