// -------------------------------------------------------------------------------
// must - Required-Dependency Panics for Constructors
//
// Author: Alex Freidah
//
// Tiny helper that constructors call on each required dep to panic at
// construction time when a wiring bug surfaces. The alternative -
// deferring to the eventual NPE inside a request-time call - hides the
// bug N call frames deep and produces stack traces that are hard to
// triage.
//
// Canonical use:
//
//   func NewRebalancer(ops Ops, store RebalancerStore) *Rebalancer {
//       must.NotNil("ops", ops)
//       must.NotNil("store", store)
//       return &Rebalancer{ops: ops, store: store}
//   }
//
// See docs/style-guide.md "Constructor Patterns" for the rule and the
// fallable-constructor exemption (I/O, parse, network).
// -------------------------------------------------------------------------------

package must

import (
	"fmt"
	"reflect"
)

// NotNil panics when v is nil. Handles both the untyped-nil case
// (v == nil) and the typed-nil interface case (e.g.
// var x Store = (*concreteStore)(nil)) which the bare comparison
// misses. name should identify the dependency for the panic message;
// constructors typically pass the parameter name.
func NotNil(name string, v any) {
	if v == nil {
		panic(fmt.Sprintf("required dependency %q is nil", name))
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		if rv.IsNil() {
			panic(fmt.Sprintf("required dependency %q is nil", name))
		}
	}
}
