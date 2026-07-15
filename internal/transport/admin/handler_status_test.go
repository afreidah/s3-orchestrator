// -------------------------------------------------------------------------------
// Admin API - Status Mapping Tests
//
// Author: Alex Freidah
//
// Unit tests for backendStatuses, the pure dashboard-to-wire mapping behind
// /admin/api/status: health inversion, drain-by-presence, per-backend counters,
// and display ordering.
// -------------------------------------------------------------------------------

package admin

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestBackendStatuses_Mapping covers the health/drain/counter mapping and that
// rows follow BackendOrder.
func TestBackendStatuses_Mapping(t *testing.T) {
	t.Parallel()
	data := &dashboard.Data{
		BackendOrder:      []string{"b1", "b2"},
		UnhealthyBackends: map[string]bool{"b2": true},
		DrainingBackends:  map[string]drain.Progress{"b1": {}},
		QuotaStats: map[string]core.QuotaStat{
			"b1": {BytesUsed: 100, BytesLimit: 1000},
		},
		ObjectCounts: map[string]int64{"b1": 7},
		UsageStats: map[string]core.UsageStat{
			"b1": {APIRequests: 3, EgressBytes: 20, IngressBytes: 40},
		},
	}

	got := backendStatuses(data)
	if len(got) != 2 || got[0].Name != "b1" || got[1].Name != "b2" {
		t.Fatalf("rows out of order: %+v", got)
	}

	// b1: healthy (absent from unhealthy), draining (present), counters mapped.
	b1 := got[0]
	if !b1.Healthy || !b1.Draining {
		t.Errorf("b1 health/drain = %v/%v, want true/true", b1.Healthy, b1.Draining)
	}
	if b1.BytesUsed != 100 || b1.BytesLimit != 1000 || b1.ObjectCount != 7 {
		t.Errorf("b1 quota/count = %+v", b1)
	}
	if b1.APIRequests != 3 || b1.EgressBytes != 20 || b1.IngressBytes != 40 {
		t.Errorf("b1 usage = %+v", b1)
	}

	// b2: unhealthy (present), not draining, and no stats seeded => zero counters.
	b2 := got[1]
	if b2.Healthy || b2.Draining {
		t.Errorf("b2 health/drain = %v/%v, want false/false", b2.Healthy, b2.Draining)
	}
	if b2.BytesUsed != 0 || b2.ObjectCount != 0 || b2.APIRequests != 0 {
		t.Errorf("b2 should have zero counters: %+v", b2)
	}
}
