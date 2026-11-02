// -------------------------------------------------------------------------------
// Usage Limits Compilation Tests
//
// Author: Alex Freidah
//
// UsageLimitsFor is where a backend's configured budgets become the limits
// admission enforces, and it is shared by startup and the reload hook so the
// two cannot disagree. What it has to get right is the desugaring: every
// deployment written before request pools existed says api_request_limit, and
// those configs must keep meaning exactly what they meant.
// -------------------------------------------------------------------------------

package core

import (
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// limitsFor compiles one backend's limits through UsageLimitsFor.
func limitsFor(t *testing.T, b *config.BackendConfig) UsageLimits {
	t.Helper()
	b.Name = "b1"
	limits, err := UsageLimitsFor([]config.BackendConfig{*b})
	if err != nil {
		t.Fatalf("UsageLimitsFor: %v", err)
	}
	return limits["b1"]
}

// TestUsageLimitsFor_BareCapDesugarsToOneWildcardPool pins backward
// compatibility: an existing api_request_limit becomes a single pool charging
// every operation, which is what the scalar always meant.
func TestUsageLimitsFor_BareCapDesugarsToOneWildcardPool(t *testing.T) {
	t.Parallel()
	lim := limitsFor(t, &config.BackendConfig{APIRequestLimit: 5000})

	pools := lim.Pools()
	if len(pools) != 1 || pools[0].Name != PoolAll || pools[0].Limit != 5000 {
		t.Fatalf("pools = %+v, want one %q pool of 5000", pools, PoolAll)
	}
	for _, op := range s3op.All() {
		if len(lim.PoolsFor(op)) != 1 {
			t.Errorf("%s charges %d pools, want 1", op, len(lim.PoolsFor(op)))
		}
	}
}

// TestUsageLimitsFor_CarriesBytesAndPools covers the full shape: byte caps stay
// scalar, and the declared pools reach the compiled limits in order.
func TestUsageLimitsFor_CarriesBytesAndPools(t *testing.T) {
	t.Parallel()
	lim := limitsFor(t, &config.BackendConfig{
		EgressByteLimit:  1 << 30,
		IngressByteLimit: 2 << 30,
		Unmetered:        []string{string(s3op.DeleteObject)},
		RequestLimits: []config.RequestPoolConfig{
			{Name: "class_a", Operations: []string{string(s3op.PutObject)}, Limit: 5000},
			{Name: "class_b", Operations: []string{string(s3op.GetObject)}, Limit: 50000},
		},
	})

	if lim.EgressByteLimit != 1<<30 || lim.IngressByteLimit != 2<<30 {
		t.Errorf("byte limits = %d/%d, want 1GiB/2GiB", lim.EgressByteLimit, lim.IngressByteLimit)
	}
	if pools := lim.PoolsFor(s3op.PutObject); len(pools) != 1 || pools[0].Name != "class_a" {
		t.Errorf("PutObject charges %+v, want class_a", pools)
	}
	if pools := lim.PoolsFor(s3op.GetObject); len(pools) != 1 || pools[0].Name != "class_b" {
		t.Errorf("GetObject charges %+v, want class_b", pools)
	}
	if pools := lim.PoolsFor(s3op.DeleteObject); len(pools) != 0 {
		t.Errorf("DeleteObject charges %+v, want nothing; the provider does not bill it", pools)
	}
}

// TestUsageLimitsFor_CarriesThePeriod verifies the configured reset day and
// zone reach the compiled limits, and that leaving both unset gives the
// calendar month in UTC.
func TestUsageLimitsFor_CarriesThePeriod(t *testing.T) {
	t.Parallel()
	lim := limitsFor(t, &config.BackendConfig{UsageResetDay: 14, UsageResetTimezone: "America/Los_Angeles"})
	if lim.Period.ResetDay != 14 || lim.Period.Location.String() != "America/Los_Angeles" {
		t.Errorf("period = %+v, want day 14 in America/Los_Angeles", lim.Period)
	}

	lim = limitsFor(t, &config.BackendConfig{})
	if lim.Period.ResetDay != 0 || lim.Period.Location != time.UTC {
		t.Errorf("default period = %+v, want day 1 (unset) in UTC", lim.Period)
	}
	if !lim.Unlimited() {
		t.Error("a backend with no configured budgets must be unlimited")
	}
}

// TestUsageLimitsFor_ErrorNamesTheBackend verifies a backend whose limits do
// not compile is named once in the error.
func TestUsageLimitsFor_ErrorNamesTheBackend(t *testing.T) {
	t.Parallel()
	_, err := UsageLimitsFor([]config.BackendConfig{{Name: "b9", UsageResetTimezone: "Nowhere/Here"}})
	if err == nil || strings.Count(err.Error(), `"b9"`) != 1 {
		t.Errorf("err = %v, want one naming b9", err)
	}
}
