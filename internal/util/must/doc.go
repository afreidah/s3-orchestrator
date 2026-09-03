// Package must provides the required-dependency panics constructors call on
// each dep, so a wiring bug surfaces at construction time.
//
// The alternative - deferring to the eventual nil dereference inside a
// request-time call - hides the bug several call frames deep and produces stack
// traces that are hard to triage.
package must
