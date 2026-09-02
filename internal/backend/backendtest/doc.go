// Package backendtest provides object-backend doubles for tests: a stateful
// in-memory backend, a failure-injecting decorator, and a latency decorator.
//
// The in-memory backend stores what is put and returns it on get, so a
// round-trip test asserts on bytes rather than on a call sequence. Reach for
// the generated MockObjectBackend when the assertion is which calls happened,
// and for this package when it is what ended up stored.
//
// Importing it from production code is not supported.
package backendtest
