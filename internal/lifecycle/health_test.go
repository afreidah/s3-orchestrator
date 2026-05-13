// -------------------------------------------------------------------------------
// Lifecycle Manager - HealthReporter Tests
//
// Author: Alex Freidah
//
// Covers Manager.Health(): registered runners that implement
// HealthReporter appear in the snapshot, services that do not are
// silently omitted, and the snapshot preserves registration order so
// operators read it in the same order as startup logs.
// -------------------------------------------------------------------------------

package lifecycle

import (
	"context"
	"testing"
	"time"
)

// reporterRunner is a Runner that also implements HealthReporter,
// used to exercise the snapshot path in Manager.Health().
type reporterRunner struct {
	health WorkerHealth
}

func (r *reporterRunner) Run(context.Context) error { return nil }
func (r *reporterRunner) Health() WorkerHealth      { return r.health }

// plainRunner is a Runner that does not implement HealthReporter.
// Used to verify Health() silently omits non-reporting services.
type plainRunner struct{}

func (plainRunner) Run(context.Context) error { return nil }

// TestManager_Health_FillsNameFromRegistration covers the contract
// that a HealthReporter returning a zero Name field inherits the
// registration name. Lets services skip setting Name themselves.
func TestManager_Health_FillsNameFromRegistration(t *testing.T) {
	t.Parallel()
	m := NewManager()
	m.Register("cleanup-queue", &reporterRunner{
		health: WorkerHealth{
			LastSuccess:         time.Unix(1, 0),
			ConsecutiveFailures: 3,
		},
	})
	snaps := m.Health()
	if len(snaps) != 1 {
		t.Fatalf("Health len = %d, want 1", len(snaps))
	}
	if snaps[0].Name != "cleanup-queue" {
		t.Errorf("Name = %q, want cleanup-queue", snaps[0].Name)
	}
	if snaps[0].ConsecutiveFailures != 3 {
		t.Errorf("ConsecutiveFailures = %d, want 3", snaps[0].ConsecutiveFailures)
	}
}

// TestManager_Health_OmitsNonReporters drives the type-assertion miss
// branch: services that do not implement HealthReporter must not
// appear in the snapshot. Without this, every registered runner would
// need to satisfy the interface.
func TestManager_Health_OmitsNonReporters(t *testing.T) {
	t.Parallel()
	m := NewManager()
	m.Register("reporter", &reporterRunner{health: WorkerHealth{Name: "reporter"}})
	m.Register("plain", plainRunner{})
	snaps := m.Health()
	if len(snaps) != 1 {
		t.Fatalf("Health len = %d, want 1 (plain runner must be skipped)", len(snaps))
	}
	if snaps[0].Name != "reporter" {
		t.Errorf("Name = %q, want reporter", snaps[0].Name)
	}
}

// TestManager_Health_PreservesRegistrationOrder pins the contract
// that the JSON dump operators read matches the startup order in
// logs. Without this, snapshot order could drift on Go map iteration
// changes.
func TestManager_Health_PreservesRegistrationOrder(t *testing.T) {
	t.Parallel()
	m := NewManager()
	for _, name := range []string{"a", "b", "c", "d"} {
		m.Register(name, &reporterRunner{health: WorkerHealth{Name: name}})
	}
	snaps := m.Health()
	for i, want := range []string{"a", "b", "c", "d"} {
		if snaps[i].Name != want {
			t.Errorf("snaps[%d].Name = %q, want %q", i, snaps[i].Name, want)
		}
	}
}
