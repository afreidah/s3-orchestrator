// -------------------------------------------------------------------------------
// DI - Batched Dependency Resolution
//
// Author: Alex Freidah
//
// Resolver lets a composite provider, or the runtime, pull many dependencies
// from the injector and check for failure once, instead of repeating the
// do.Invoke/if-err/return block per dependency. It records the first error and
// short-circuits, so resolution stops at the first missing dependency.
// -------------------------------------------------------------------------------

package di

import (
	"fmt"

	"github.com/samber/do/v2"
)

// Resolver batches do.Invoke calls against one injector, remembering the first
// error. Once it has failed, later resolve calls return the zero value without
// invoking, so a caller resolves its whole dependency set then checks the
// error once before building.
type Resolver struct {
	inj do.Injector
	err error
}

// NewResolver starts a batch against inj.
func NewResolver(i do.Injector) *Resolver { return &Resolver{inj: i} }

// Resolve pulls T, or returns the zero value if the resolver has already
// failed. The first failure is retained, wrapped as "resolve <type>: %w" so a
// missing provider points at the dependency that failed.
func (r *Resolver) Resolve[T any]() T {
	if r.err != nil {
		var zero T
		return zero
	}
	v, err := do.Invoke[T](r.inj)
	if err != nil {
		r.err = fmt.Errorf("resolve %s: %w", do.NameOf[T](), err)
	}
	return v // already the zero value when err != nil
}

// Err returns the first resolution failure, or nil when every dependency
// resolved.
func (r *Resolver) Err() error { return r.err }
