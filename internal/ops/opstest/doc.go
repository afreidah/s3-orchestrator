// Package opstest hosts the mockgen-generated mocks for the consumer-defined
// interfaces in internal/ops. They live in their own package so the transport
// tests can stand an operations layer up over fakes, rather than each package
// hand-rolling its own stub of the same worker.
package opstest
