//go:build integration

// Package integration holds the end-to-end suite that runs against a real
// Postgres and real object backends, behind the "integration" build tag.
//
// Tests take one of two fixtures. The shared one in helpers_test.go hands every
// test the same database, backend fleet and quota rows. A test that needs a
// different fleet builds a harness instead: its own database, buckets, manager,
// workers and proxy, so nothing it does is visible to another test and no state
// has to be saved and restored.
package integration
