// Package store builds the database circuit breaker, the error filter the
// driver-level wrappers apply, and the ping-driven recovery that closes it.
//
// The breaker is a first-class DI-registered value; consumers needing its
// lifecycle controls invoke *breaker.CircuitBreaker directly. The metadata
// store implementations live in the core, postgres and sqlite subpackages.
package store
