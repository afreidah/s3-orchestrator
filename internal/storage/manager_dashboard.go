// -------------------------------------------------------------------------------
// Dashboard Data - Aggregated Stats for the Web UI
//
// Author: Alex Freidah
//
// Exposes a single GetDashboardData method that fetches all operational stats
// needed for the web dashboard in one call. Delegates to the underlying
// MetadataStore, benefiting from the circuit breaker when wired through
// CircuitBreakerStore.
// -------------------------------------------------------------------------------

package storage

import (
	"context"
	"strings"
)

// DashboardData holds a snapshot of all operational data for the dashboard.
type DashboardData struct {
	BackendOrder          []string
	QuotaStats            map[string]QuotaStat
	ObjectCounts          map[string]int64
	ActiveMultipartCounts map[string]int64
	UsageStats            map[string]UsageStat
	UsageLimits           map[string]UsageLimits
	UsagePeriod           string
	Objects               []DashboardObject
}

// DashboardObject is a flattened view of an object for the UI.
type DashboardObject struct {
	Bucket    string
	Key       string
	Backend   string
	SizeBytes int64
	CreatedAt string // formatted as "2006-01-02 15:04"
}

// GetDashboardData fetches all stats needed for the web UI in one call.
func (m *BackendManager) GetDashboardData(ctx context.Context) (*DashboardData, error) {
	data := &DashboardData{
		BackendOrder: m.order,
		UsageLimits:  m.usageLimits,
		UsagePeriod:  currentPeriod(),
	}

	var err error

	data.QuotaStats, err = m.store.GetQuotaStats(ctx)
	if err != nil {
		return nil, err
	}

	data.ObjectCounts, err = m.store.GetObjectCounts(ctx)
	if err != nil {
		return nil, err
	}

	data.ActiveMultipartCounts, err = m.store.GetActiveMultipartCounts(ctx)
	if err != nil {
		return nil, err
	}

	data.UsageStats, err = m.store.GetUsageForPeriod(ctx, data.UsagePeriod)
	if err != nil {
		return nil, err
	}

	// Fetch all objects (up to 1000) for the file listing.
	listResult, err := m.store.ListObjects(ctx, "", "", 1000)
	if err != nil {
		return nil, err
	}
	data.Objects = make([]DashboardObject, len(listResult.Objects))
	for i, obj := range listResult.Objects {
		bucket, key, _ := strings.Cut(obj.ObjectKey, "/")
		data.Objects[i] = DashboardObject{
			Bucket:    bucket,
			Key:       key,
			Backend:   obj.BackendName,
			SizeBytes: obj.SizeBytes,
			CreatedAt: obj.CreatedAt.Format("2006-01-02 15:04"),
		}
	}

	return data, nil
}
