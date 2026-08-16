// -------------------------------------------------------------------------------
// Admin API - Replication Status Handler Tests
//
// Author: Alex Freidah
//
// Covers the replication-status endpoint: the typed snapshot response, 503 when
// the collector is not wired, and 503 before the first snapshot is computed.
// -------------------------------------------------------------------------------

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// fakeReplication is a replicationSnapshotter returning a canned snapshot.
type fakeReplication struct{ snap metrics.ReplicationSnapshot }

func (f fakeReplication) ReplicationSnapshot() metrics.ReplicationSnapshot { return f.snap }

func TestHandleReplicationStatus_ReturnsSnapshot(t *testing.T) {
	t.Parallel()
	h := &Handler{replMetrics: fakeReplication{snap: metrics.ReplicationSnapshot{
		Factor: 2, UnderReplicated: 143, OverReplicated: 12, ComputedAt: time.Now(), Ready: true,
	}}}

	w := httptest.NewRecorder()
	h.handleReplicationStatus(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/replication", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.ReplicationStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Factor != 2 || resp.UnderReplicated != 143 || resp.OverReplicated != 12 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestHandleReplicationStatus_NotWiredReturns503(t *testing.T) {
	t.Parallel()
	h := &Handler{} // replication nil
	w := httptest.NewRecorder()
	h.handleReplicationStatus(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/replication", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestHandleReplicationStatus_NotReadyReturns503(t *testing.T) {
	t.Parallel()
	h := &Handler{replMetrics: fakeReplication{snap: metrics.ReplicationSnapshot{Ready: false}}}
	w := httptest.NewRecorder()
	h.handleReplicationStatus(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/replication", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}
