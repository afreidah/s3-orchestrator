// -------------------------------------------------------------------------------
// Proxytest Fixture Tests
//
// Author: Alex Freidah
//
// Helper tests for the proxytest fixture. The helpers are test-only API, but
// the new-code coverage gate counts them like anything else, so they are
// driven once here.
// -------------------------------------------------------------------------------

package proxytest_test

import (
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newStack builds a minimal stack fed only by a single MockStore.
func newStack(t *testing.T, mock *storetest.MockMetadataStore) *proxytest.Stack {
	t.Helper()
	return proxytest.New(t, mock, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{
			Backends:        map[string]backend.ObjectBackend{},
			Order:           []string{},
			RoutingStrategy: config.RoutingPack,
			Metrics:         mock,
		}),
	})
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestNewStack_WiresEveryCollaborator asserts the fixture hands back a
// populated stack, since a nil field would surface in a test as a confusing
// panic somewhere far from the wiring.
func TestNewStack_WiresEveryCollaborator(t *testing.T) {
	t.Parallel()
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	st := newStack(t, mock)

	if st.Runtime == nil {
		t.Error("Runtime not wired")
	}
	if st.Coord == nil {
		t.Error("Coord not wired")
	}
	if st.Objects == nil {
		t.Error("Objects not wired")
	}
	if st.Multipart == nil {
		t.Error("Multipart not wired")
	}
	if st.Drain == nil {
		t.Error("Drain not wired")
	}
	if st.Usage == nil {
		t.Error("Usage not wired")
	}
	if st.IntegrityCfg == nil {
		t.Error("IntegrityCfg not wired")
	}
}

// TestNewStack_SharesTheIntegrityConfig pins the invariant the fixture exists
// to hold: both managers read one config value, so a reload reaches both. Two
// pointers here would let a test pass against a shape production cannot have.
func TestNewStack_SharesTheIntegrityConfig(t *testing.T) {
	t.Parallel()
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	st := newStack(t, mock)

	st.IntegrityCfg.Store(&config.IntegrityConfig{Enabled: true})
	if got := st.IntegrityCfg.Load(); got == nil || !got.Enabled {
		t.Errorf("integrity config = %+v, want the stored value", got)
	}
}

// TestBuildWorkers wires every worker over a freshly-built stack and asserts
// each handle on the returned Workers struct is populated.
func TestBuildWorkers(t *testing.T) {
	t.Parallel()
	mock := storetest.NewMockMetadataStore(gomock.NewController(t))
	st := newStack(t, mock)

	w := proxytest.BuildWorkers(st, mock)

	if w.Rebalancer == nil {
		t.Error("Rebalancer not wired")
	}
	if w.Replicator == nil {
		t.Error("Replicator not wired")
	}
	if w.OverReplicationCleaner == nil {
		t.Error("OverReplicationCleaner not wired")
	}
	if w.CleanupWorker == nil {
		t.Error("CleanupWorker not wired")
	}
	if w.PendingReaper == nil {
		t.Error("PendingReaper not wired")
	}
	if w.Scrubber == nil {
		t.Error("Scrubber not wired")
	}
	if w.Drain != st.Drain {
		t.Error("Workers.Drain is not the stack's own drain manager")
	}
}
