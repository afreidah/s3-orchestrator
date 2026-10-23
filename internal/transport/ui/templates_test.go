// -------------------------------------------------------------------------------
// Template Helper Tests
//
// Author: Alex Freidah
//
// Tests for dashboard template utility functions. Validates byte formatting,
// percentage calculations, and other display helper outputs.
// -------------------------------------------------------------------------------

package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/afreidah/s3-orchestrator/internal/proxy/backendstate"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestDashboard_BadgesCordonedBackends renders the backends table and verifies
// a cordoned backend carries the Cordoned badge, a cordoned and draining one
// carries both badges, and a backend in service carries neither.
func TestDashboard_BadgesCordonedBackends(t *testing.T) {
	t.Parallel()
	rowFor := renderBackendRows(t, &dashboard.Data{
		BackendOrder: []string{"held", "both", "plain"},
		BackendFlags: map[string]core.BackendFlags{
			"held": core.FlagCordoned,
			"both": core.FlagCordoned | core.FlagDraining,
		},
		DrainingBackends: map[string]backendstate.DrainProgress{"both": {Active: true, State: "draining"}},
	})
	if row := rowFor("held"); !strings.Contains(row, "badge-cordoned") || strings.Contains(row, "badge-draining") {
		t.Errorf("held row = %s, want only the cordoned badge", row)
	}
	if row := rowFor("both"); !strings.Contains(row, "badge-cordoned") || !strings.Contains(row, "badge-draining") {
		t.Errorf("both row = %s, want the cordoned and draining badges", row)
	}
	if row := rowFor("plain"); strings.Contains(row, "badge-") {
		t.Errorf("plain row = %s, want no badge", row)
	}
	if row := rowFor("held"); !strings.Contains(row, `class="row-cordoned"`) {
		t.Errorf("held row = %s, want it highlighted as cordoned", row)
	}
}

// TestDashboard_BadgesRefusingBackends verifies a backend refusing some calls
// is badged Degraded with the calls named, one refusing every call is badged
// Down, and one refusing nothing carries no badge.
func TestDashboard_BadgesRefusingBackends(t *testing.T) {
	t.Parallel()
	rowFor := renderBackendRows(t, &dashboard.Data{
		BackendOrder: []string{"partial", "down", "fine"},
		RefusedCalls: map[string]core.BackendCalls{
			"partial": core.CallGetObject | core.CallListObjects,
			"down":    core.AllCalls,
		},
	})
	if row := rowFor("partial"); !strings.Contains(row, `class="row-degraded"`) || !strings.Contains(row, "Degraded: GetObject,ListObjects") {
		t.Errorf("partial row = %s, want it degraded and naming GetObject and ListObjects", row)
	}
	if row := rowFor("down"); !strings.Contains(row, `class="row-down"`) || !strings.Contains(row, "badge-down") || strings.Contains(row, "Degraded") {
		t.Errorf("down row = %s, want it down and not degraded", row)
	}
	if row := rowFor("fine"); strings.Contains(row, "badge-") || strings.Contains(row, "class=") {
		t.Errorf("fine row = %s, want no badge or highlight", row)
	}
}

// TestDashboard_UsageShowsEachBackendsCycle verifies the Usage section labels
// each backend's row with its own cycle rather than one fleet-wide month.
func TestDashboard_UsageShowsEachBackendsCycle(t *testing.T) {
	t.Parallel()
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	data := &dashboard.Data{
		BackendOrder: []string{"c2", "r2"},
		UsageCycles: map[string]core.UsageCycle{
			"c2": core.UsagePeriod{ResetDay: 15}.Cycle(now),
			"r2": core.UsagePeriod{Location: la}.Cycle(now),
		},
		UsagePeriod:     "2026-10",
		TopLevelEntries: &core.DirectoryListResult{},
	}
	var buf bytes.Buffer
	if err := loadTemplates().ExecuteTemplate(&buf, "dashboard.html", dashboardPage{Data: data}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	_, rest, found := strings.Cut(out, "<h2>Usage</h2>")
	section, _, ok := strings.Cut(rest, "</section>")
	if !found || !ok || strings.Contains(out, "Monthly Usage") {
		t.Fatalf("no Usage section, or the old Monthly Usage heading remains:\n%s", out)
	}
	for _, want := range []string{"<td>c2</td>", "Oct 15 - Nov 15 UTC", "<td>r2</td>", "Oct 1 - Nov 1 America/Los_Angeles"} {
		if !strings.Contains(section, want) {
			t.Errorf("Usage section missing %q:\n%s", want, section)
		}
	}
}

// renderBackendRows renders the dashboard over data and returns a lookup from
// a backend's name to its table row, from the row's opening tag through the
// name cell, which is where the row highlight and every badge are.
func renderBackendRows(t *testing.T, data *dashboard.Data) func(name string) string {
	t.Helper()
	data.TopLevelEntries = &core.DirectoryListResult{}
	var buf bytes.Buffer
	if err := loadTemplates().ExecuteTemplate(&buf, "dashboard.html", dashboardPage{Data: data}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	return func(name string) string {
		cell := strings.Index(out, "<td>"+name)
		if cell < 0 {
			t.Fatalf("no row for %s", name)
		}
		start := strings.LastIndex(out[:cell], "<tr")
		return out[start : cell+strings.Index(out[cell:], "</td>")]
	}
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// newTestCounterVec returns a fresh CounterVec backed by an isolated
// registry so the counterVecTotal test does not interact with the
// global telemetry counters. The vec is unregistered - the helper only
// needs the Collect() surface, which works on any prometheus.Collector.
func newTestCounterVec() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_counter_vec_total",
		Help: "isolated counter for counterVecTotal tests",
	}, []string{"operation"})
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestCounterVecTotal exercises the snapshot helper that surfaces
// integrity check / error totals on the dashboard. Uses an isolated
// CounterVec so the test does not depend on the live telemetry
// counters or the order in which test packages register them.
func TestCounterVecTotal(t *testing.T) {
	t.Parallel()
	vec := newTestCounterVec()

	if got := counterVecTotal(vec); got != 0 {
		t.Errorf("empty vec total = %v, want 0", got)
	}

	vec.WithLabelValues("read").Inc()
	vec.WithLabelValues("read").Inc()
	vec.WithLabelValues("scrub").Inc()

	if got := counterVecTotal(vec); got != 3 {
		t.Errorf("populated vec total = %v, want 3 (2 read + 1 scrub)", got)
	}
}

// TestPct verifies the pct contract.
// Asserts that pct(, ) = , want.
func TestPct(t *testing.T) {
	t.Parallel()
	tests := []struct {
		used, limit int64
		want        string
	}{
		{50, 100, "50.0%"},
		{1, 3, "33.3%"},
		{0, 100, "0.0%"},
		{100, 100, "100.0%"},
		{0, 0, "unlimited"},
		{500, 0, "unlimited"},
	}

	for _, tt := range tests {
		got := pct(tt.used, tt.limit)
		if got != tt.want {
			t.Errorf("pct(%d, %d) = %q, want %q", tt.used, tt.limit, got, tt.want)
		}
	}
}

// TestPctFloat verifies the pct float contract.
// Asserts that pctFloat(, ) = , want.
func TestPctFloat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		used, limit int64
		want        float64
	}{
		{50, 100, 50.0},
		{0, 100, 0.0},
		{100, 100, 100.0},
		{150, 100, 100.0}, // capped at 100
		{0, 0, 0.0},       // unlimited
		{500, 0, 0.0},     // unlimited
	}

	for _, tt := range tests {
		got := pctFloat(tt.used, tt.limit)
		if got != tt.want {
			t.Errorf("pctFloat(%d, %d) = %f, want %f", tt.used, tt.limit, got, tt.want)
		}
	}
}

// TestBarClass verifies each usage band gets its bar class: ok under 70%,
// warn from 70%, error from 90%, and unlimited with no limit.
func TestBarClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		used, limit int64
		want        string
	}{
		{50, 100, "bar-ok"},
		{69, 100, "bar-ok"},
		{70, 100, "bar-warn"},
		{85, 100, "bar-warn"},
		{90, 100, "bar-error"},
		{100, 100, "bar-error"},
		{0, 0, "bar-unlimited"},
		{500, 0, "bar-unlimited"},
	}

	for _, tt := range tests {
		got := barClass(tt.used, tt.limit)
		if got != tt.want {
			t.Errorf("barClass(%d, %d) = %q, want %q", tt.used, tt.limit, got, tt.want)
		}
	}
}

// TestSub covers the subtraction the savings figures rely on, including the
// negative case: an encoding that grew is a real state on a fleet where the
// ratio floor was widened after the fact, and the page must show it rather than
// clamp it to zero.
func TestSub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"a saving", 1000, 250, 750},
		{"nothing saved", 500, 500, 0},
		{"grew", 500, 600, -100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sub(tt.a, tt.b); got != tt.want {
				t.Errorf("sub(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestRatio covers the compression ratio cell, whose interesting case is an
// empty fleet: nothing measured is a dash, not a zero, since a zero there reads
// as an encoder achieving perfect compression.
func TestRatio(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b int64
		want string
	}{
		{"quarter", 250, 1000, "0.25"},
		{"no saving", 1000, 1000, "1.00"},
		{"nothing measured", 0, 0, "-"},
		{"stored without logical", 100, 0, "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ratio(tt.a, tt.b); got != tt.want {
				t.Errorf("ratio(%d, %d) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
