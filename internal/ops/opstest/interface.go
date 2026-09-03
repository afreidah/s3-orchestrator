// -------------------------------------------------------------------------------
// Ops Test Doubles - Mock Generation Surface
//
// Author: Alex Freidah
//
// Declares the go:generate directive that builds mocks for every
// consumer-defined interface in internal/ops. The mocks live in their own
// package so a transport test can stand up an operations layer over fakes
// rather than hand-rolling a stub of the same worker in each package.
// -------------------------------------------------------------------------------

package opstest

//go:generate mockgen -destination=mocks.go -package=opstest github.com/afreidah/s3-orchestrator/internal/ops ObjectAPI,ObjectStore,UsageGate,IntegrityConfigLoader,RuntimeOps,ReplicatorOps,RebalancerOps,OverReplicationOps,ScrubberOps,EncryptionStore,CompressionStore
