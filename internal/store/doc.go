// Package store builds the database circuit breaker and the error filter the
// driver-level wrappers apply.
//
// The breaker is a first-class DI-registered value; consumers needing its
// lifecycle controls invoke *breaker.CircuitBreaker directly. The metadata
// store implementations live in the core, postgres and sqlite subpackages.
package store
