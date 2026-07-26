// -------------------------------------------------------------------------------
// DI - Batched Dependency Resolution
//
// Author: Alex Freidah
//
// resolver lets a composite provider pull many dependencies from the injector
// and check for failure once, instead of repeating the
// do.Invoke/if-err/return block per dependency. It records the first error and
// short-circuits, so resolution stops at the first missing dependency exactly
// as the sequential form did.
// -------------------------------------------------------------------------------

package di

import (
	"fmt"

	"github.com/samber/do/v2"
)

// resolver batches do.Invoke calls against one injector, remembering the first
// error. Once it has failed, later resolve calls return the zero value without
// invoking, so a provider resolves its whole dependency set then checks err
// once before building.
type resolver struct {
	inj do.Injector
	err error
}

// newResolver starts a batch against inj.
func newResolver(i do.Injector) *resolver { return &resolver{inj: i} }

// resolve pulls T, or returns the zero value if the resolver has already failed.
// The first failure is retained on the resolver for the caller to check.
func resolve[T any](r *resolver) T {
	if r.err != nil {
		var zero T
		return zero
	}
	v, err := do.Invoke[T](r.inj)
	if err != nil {
		r.err = err
	}
	return v // already the zero value when err != nil
}

// resolveNamed resolves T like resolve, but on the first failure wraps the
// error as "resolve <name>: %w" so a missing provider points at the right
// dependency. Later calls short-circuit once the resolver has failed. Use
// this where the sequential form wrapped its errors with the dependency name;
// use plain resolve where it returned the bare do.Invoke error.
func resolveNamed[T any](r *resolver, name string) T {
	if r.err != nil {
		var zero T
		return zero
	}
	v, err := do.Invoke[T](r.inj)
	if err != nil {
		r.err = fmt.Errorf("resolve %s: %w", name, err)
	}
	return v // already the zero value when err != nil
}
