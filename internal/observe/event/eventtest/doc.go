// Package eventtest provides test helpers for code that publishes notification
// events. Importing it from production code is not supported.
//
// The emitter is process-global, so a test that records events cannot run in
// parallel with another that publishes them.
package eventtest
