// -------------------------------------------------------------------------------
// Shared Transactional Store Methods
//
// Author: Alex Freidah
//
// The store methods whose whole implementation is a transaction composed in
// this package are methods on TxOps, declared beside the logic they run. The
// engines contribute nothing to them beyond being the Runner that supplies the
// TxAdapter, so each engine store embeds TxOps rather than restating them.
//
// An engine that needs to diverge declares the method on its own Store, which
// shadows the promoted one.
// -------------------------------------------------------------------------------

package core

import (
	"golang.org/x/sync/semaphore"
)

// TxOps holds the Runner that the shared methods run their transactions
// against, the slots that bound how many claims run at once, and where the
// replication factors its writes queue keys against come from. It is embedded
// by each engine store, which is itself that Runner.
type TxOps struct {
	runner     Runner
	claimSlots *semaphore.Weighted
	factors    *FactorsHolder
}

// NewTxOps binds the shared methods to the store that will embed them. A claim
// waiting on a backend's lock holds a connection, so claimSlots is the most of
// them the store's claims may occupy; the rest wait without one.
func NewTxOps(r Runner, claimSlots int) TxOps {
	return TxOps{runner: r, claimSlots: semaphore.NewWeighted(int64(claimSlots)), factors: &FactorsHolder{}}
}

// UseReplicationFactors names where the store's writes read the replication
// factors, so each reads the running values. Until it is called, writes queue
// nothing.
func (o TxOps) UseReplicationFactors(factors func() ReplicationFactors) {
	o.factors.Use(factors)
}

// ReplicationFactors returns the factors the store's writes queue keys
// against now, none before they are named.
func (o TxOps) ReplicationFactors() ReplicationFactors {
	return o.factors.Load()
}

// edit starts the ledger edit of one transaction, queueing keys against the
// running replication factors.
func (o TxOps) edit(tx TxAdapter) *ledgerEdit {
	return newLedgerEdit(tx, o.ReplicationFactors())
}
