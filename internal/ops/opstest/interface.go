// Package opstest hosts the mockgen-generated mocks for the consumer-defined
// interfaces in internal/ops. They live in their own package so the transport
// tests can stand an operations layer up over fakes, rather than each package
// hand-rolling its own stub of the same worker.
package opstest

//go:generate mockgen -destination=mocks.go -package=opstest github.com/afreidah/s3-orchestrator/internal/ops ObjectAPI,ObjectStore,BackendOps,RuntimeOps,ReplicatorOps,RebalancerOps,OverReplicationOps,ScrubberOps,EncryptionStore
