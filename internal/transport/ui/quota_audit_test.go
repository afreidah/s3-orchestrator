// -------------------------------------------------------------------------------
// Quota Tracking Audit Guard
//
// Author: Alex Freidah
//
// Forces every UI API route registered on the dashboard mux to be classified
// against quota tracking. New routes that touch a real backend without an
// audit entry stating where their usage.Record/IncrementQuota site lives
// fail this test, blocking the PR until the developer documents tracking.
// -------------------------------------------------------------------------------

package ui

import (
	"strings"
	"testing"
)

// TestUIAPIRoutes_QuotaTrackingClassified guards against new UI API routes
// being added without a tracking classification. Each tracked route must
// also carry a non-empty audit string pointing at the recording site.
func TestUIAPIRoutes_QuotaTrackingClassified(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, len(uiAPIRoutes))
	for _, route := range uiAPIRoutes {
		if seen[route.suffix] {
			t.Errorf("uiAPIRoutes has duplicate suffix %q", route.suffix)
		}
		seen[route.suffix] = true

		if !strings.HasPrefix(route.suffix, "/api/") {
			t.Errorf("route suffix %q does not start with /api/", route.suffix)
		}

		switch route.tracking {
		case quotaTrackingNone:
			if route.audit != "" {
				t.Errorf("route %q is classified as untracked but carries an audit string; either drop the audit or change the tracking class", route.suffix)
			}
		case quotaTrackingTracked:
			if strings.TrimSpace(route.audit) == "" {
				t.Errorf("route %q is classified as tracked but has no audit note pointing at the usage.Record site", route.suffix)
			}
		default:
			t.Errorf("route %q has unknown quotaTracking value %d", route.suffix, route.tracking)
		}
	}
}