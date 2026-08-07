// -------------------------------------------------------------------------------
// Drain Manager - Branch Tests
//
// Author: Alex Freidah
//
// Targeted unit coverage for the drain manager's move-dispatch wiring. The
// full drain lifecycle is exercised by the integration suite; this pins the
// slow-path branch's reason-profile selection in isolation.
// -------------------------------------------------------------------------------

package drain

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// captureMover records the MoveRequest the drain manager dispatches so the
// reason-profile wiring can be asserted without the full write coordinator.
type captureMover struct{ req *writepath.MoveRequest }

func (m *captureMover) DeleteOrEnqueue(context.Context, backend.ObjectBackend, string, string, string, int64) {
}

func (m *captureMover) MoveObject(_ context.Context, req *writepath.MoveRequest) (int64, error) {
	m.req = req
	return req.SizeBytes, nil
}

// TestCopyAndRemoveSource_UsesDrainMoveReasons verifies the slow-path drain
// branch (no replica: stream the object to a destination) dispatches MoveObject
// with the drain reason profile and the chosen src/dest backends.
func TestCopyAndRemoveSource_UsesDrainMoveReasons(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)

	srcBe := backendtest.NewMockObjectBackend(ctrl)
	destBe := backendtest.NewMockObjectBackend(ctrl)
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"src", "dest"}), nil)
	inf := infra.New(&infra.Config{
		Backends: map[string]backend.ObjectBackend{"src": srcBe, "dest": destBe},
		Order:    []string{"src", "dest"},
		Usage:    usage,
	})

	// Each drain dependency takes its own role: only the quota store is
	// exercised here, and an unexpected call to either of the others fails the
	// test rather than being silently absorbed.
	objects := storetest.NewMockObjectStore(ctrl)
	quota := storetest.NewMockQuotaStore(ctrl)
	backendLifecycle := storetest.NewMockBackendLifecycleStore(ctrl)
	quota.EXPECT().GetLeastUtilizedBackend(gomock.Any(), int64(50), gomock.Any()).Return("dest", nil)

	mover := &captureMover{}
	mgr := New(inf, mover, objects, quota, backendLifecycle,
		func(context.Context, string) {},
		func(context.Context) (int, int) { return 0, 0 })

	obj := &core.ObjectLocation{ObjectKey: "k", SizeBytes: 50, BackendName: "src"}
	if !mgr.copyAndRemoveSource(context.Background(), srcBe, "src", obj) {
		t.Fatal("copyAndRemoveSource returned false, want true")
	}
	if mover.req == nil || mover.req.Reasons != writepath.DrainMoveReasons {
		t.Fatalf("MoveRequest.Reasons = %+v, want DrainMoveReasons", mover.req)
	}
	if mover.req.SrcName != "src" || mover.req.DestName != "dest" {
		t.Errorf("move src/dest = %q/%q, want src/dest", mover.req.SrcName, mover.req.DestName)
	}
}
