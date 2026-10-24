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

// TxOps holds the Runner that the shared methods run their transactions
// against. It is embedded by each engine store, which is itself that Runner.
type TxOps struct {
	runner Runner
}

// NewTxOps binds the shared methods to the store that will embed them.
func NewTxOps(r Runner) TxOps { return TxOps{runner: r} }
