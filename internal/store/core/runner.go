// -------------------------------------------------------------------------------
// Core Runner - Generic Transaction Helper
//
// Author: Alex Freidah
//
// Runner is the engine-provided entry point for transactional work. Engine
// packages return one and core operations call WithTx with a closure that
// receives a TxAdapter scoped to the open transaction. The generic return
// type lets the closure surface a value without the captured-result trick.
// -------------------------------------------------------------------------------

package core

import "context"

// -------------------------------------------------------------------------
// RUNNER INTERFACE
// -------------------------------------------------------------------------

// Runner opens a transaction and invokes fn with a TxAdapter scoped to
// it. The transaction commits if fn returns a nil error and rolls back
// otherwise.
//
// WithTx is declared in method form because a method cannot carry its own type
// parameter. Engines implement this unwrapped signature; core operations call
// the WithTxVal helper below, which wraps it to return a value.
type Runner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx TxAdapter) error) error
}

// -------------------------------------------------------------------------
// GENERIC HELPER
// -------------------------------------------------------------------------

// WithTxVal opens a transaction via runner and invokes fn. If fn
// returns an error, the transaction is rolled back and the zero value
// of T is returned. Otherwise the transaction commits and fn's return
// value is propagated. This is the calling convention core operations
// use to surface a value out of a transactional body.
func WithTxVal[T any](ctx context.Context, runner Runner, fn func(ctx context.Context, tx TxAdapter) (T, error)) (T, error) {
	var out T
	err := runner.WithTx(ctx, func(ctx context.Context, tx TxAdapter) error {
		v, ferr := fn(ctx, tx)
		if ferr != nil {
			return ferr
		}
		out = v
		return nil
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}
