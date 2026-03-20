// -------------------------------------------------------------------------------
// Ops Interface — Worker Dependency Contract
//
// Author: Alex Freidah
//
// Defines the operational primitives that background workers need from the
// proxy layer. Workers receive this interface instead of embedding backendCore.
// The proxy.BackendManager implements it.
// -------------------------------------------------------------------------------

// Package worker contains background services that maintain storage health:
// rebalancing, replication, over-replication cleanup, orphan cleanup queue
// processing, and reconciliation. Each worker receives an Ops interface
// for its dependencies rather than embedding shared infrastructure.
package worker

//go:generate mockgen -destination=mock_ops_test.go -package=worker github.com/afreidah/s3-orchestrator/internal/worker Ops,BackendSyncer

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// Ops provides the shared operational primitives that background workers need.
// Implemented by proxy.BackendManager.
type Ops interface {
	// Backend access
	GetBackend(name string) (backend.ObjectBackend, error)
	Backends() map[string]backend.ObjectBackend
	BackendOrder() []string

	// Store access
	Store() store.MetadataStore

	// Usage tracking
	Usage() *counter.UsageTracker

	// Admission control
	AcquireAdmission(ctx context.Context) bool
	ReleaseAdmission()

	// Timeouts
	WithTimeout(ctx context.Context) (context.Context, context.CancelFunc)

	// Data movement helpers
	StreamCopy(ctx context.Context, src, dst backend.ObjectBackend, key string) error
	DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) error
	DeleteOrEnqueue(ctx context.Context, be backend.ObjectBackend, backendName, key, reason string, sizeBytes int64)

	// Metrics
	RecordOperation(operation, backendName string, start time.Time, err error)

	// Drain state
	IsDraining(name string) bool
	ExcludeDraining(eligible []string) []string
}
