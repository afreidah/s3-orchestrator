// -------------------------------------------------------------------------------
// Admin API - Replication Status Handler Tests
//
// Author: Alex Freidah
//
// Covers the replication-status endpoint: both sides of the queue with the
// running factor, the zeroed counts of a disabled replication, and a failed
// count.
// -------------------------------------------------------------------------------

package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// replicationStatus serves the replication-status endpoint and returns the
// response.
func replicationStatus(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.handleReplicationStatus(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/replication", nil))
	return w
}

// TestHandleReplicationStatus_ReportsTheQueue verifies the counts are both
// sides of the replication queue, with the running factor.
func TestHandleReplicationStatus_ReportsTheQueue(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	replicationWith(t, h, replicatorStub{pending: 143}, overRepStub{cfg: &config.ReplicationConfig{Factor: 2}, count: 12})

	w := replicationStatus(t, h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp adminapi.ReplicationStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Factor != 2 || resp.UnderReplicated != 143 || resp.OverReplicated != 12 || resp.ComputedAt.IsZero() {
		t.Errorf("resp = %+v, want factor 2, 143 under, 12 over, stamped", resp)
	}
}

// TestHandleReplicationStatus_Disabled verifies a disabled replication reports
// its factor with zeroed counts rather than an error.
func TestHandleReplicationStatus_Disabled(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	replicationWith(t, h, replicatorStub{}, overRepStub{cfg: &config.ReplicationConfig{Factor: 1}})

	w := replicationStatus(t, h)
	var resp adminapi.ReplicationStatusResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp.Factor != 1 || resp.UnderReplicated != 0 || resp.OverReplicated != 0 {
		t.Errorf("status = %d, resp = %+v; want 200 with factor 1 and no counts", w.Code, resp)
	}
}

// TestHandleReplicationStatus_CountError verifies a failed count answers 500.
func TestHandleReplicationStatus_CountError(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	replicationWith(t, h, replicatorStub{}, overRepStub{cfg: &config.ReplicationConfig{Factor: 2}, countErr: errors.New("db down")})

	if w := replicationStatus(t, h); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}
