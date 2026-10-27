// -------------------------------------------------------------------------------
// CLI Output - Ceilings
//
// Author: Alex Freidah
//
// A ceiling is one limit a backend is held to: its storage quota or one of its
// usage limits, with what it has used and the units its figures render in.
// admin status and the TUI build their limit rows from the same ceilings and
// agree on which one across the fleet is nearest full.
// -------------------------------------------------------------------------------

package output

import (
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
)

// StorageLabel names the ceiling a backend's storage quota sets.
const StorageLabel = "storage"

// NoLimits is what a fleet with no limit anywhere reports in place of its
// nearest one.
const NoLimits = "no limits set"

// Ceiling is one limit and what has been used of it. Label names the limit,
// and Format renders its figures: bytes, or a call count for a request
// budget. A limit of zero or less means there is none.
type Ceiling struct {
	Label  string
	Used   int64
	Limit  int64
	Format func(int64) string
}

// StorageCeiling is a backend's stored bytes against its quota.
// A backend with no quota yields a ceiling with no limit.
func StorageCeiling(b *adminapi.BackendStatus) Ceiling {
	return Ceiling{Label: StorageLabel, Used: b.BytesUsed, Limit: b.BytesLimit, Format: humanize.Bytes}
}

// UsageCeiling is one usage limit a backend enforces. It is named by its kind,
// and a request budget by kind and budget as "requests:<budget>"; egress and
// ingress count bytes and a request budget counts calls.
func UsageCeiling(u adminapi.UsageMeter) Ceiling {
	c := Ceiling{Label: u.Kind, Used: u.Used, Limit: u.Limit, Format: humanize.Bytes}
	if u.Budget != "" {
		c.Label = u.Kind + ":" + u.Budget
	}
	if u.Kind == core.MeterRequests {
		c.Format = humanize.Comma
	}
	return c
}

// Share is the fraction of the limit used, unrounded so two ceilings at the
// same whole percentage still order. A ceiling with no limit has used none.
func (c Ceiling) Share() float64 {
	if c.Limit <= 0 {
		return 0
	}
	return float64(c.Used) / float64(c.Limit)
}

// Percent is the share of the limit used as a whole-number percentage, and 0
// for a ceiling with no limit.
func (c Ceiling) Percent() int {
	return humanize.Percent(c.Used, c.Limit)
}

// PercentText renders the percentage used as "N%", and a dash for a ceiling
// with no limit, so a table cell is never blank.
func (c Ceiling) PercentText() string {
	if c.Limit <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", c.Percent())
}

// Figures renders what has been used against the limit, as "used / limit".
func (c Ceiling) Figures() string {
	return c.Format(c.Used) + " / " + c.Format(c.Limit)
}

// BindingLimit is the ceiling nearest full across a fleet, the backend it
// belongs to and when that backend's usage cycle resets it. A storage quota
// never resets, so its Reset is zero.
type BindingLimit struct {
	Ceiling
	Backend string
	Reset   time.Time
}

// NearestLimit finds the ceiling with the largest share used across every
// backend's storage quota and usage limits. It reports false when no backend
// has a limit; on a tie the earlier backend wins.
func NearestLimit(backends []adminapi.BackendStatus) (BindingLimit, bool) {
	var nearest BindingLimit
	found := false
	consider := func(b *adminapi.BackendStatus, c Ceiling, reset time.Time) {
		if c.Limit > 0 && (!found || c.Share() > nearest.Share()) {
			nearest, found = BindingLimit{Ceiling: c, Backend: b.Name, Reset: reset}, true
		}
	}
	for i := range backends {
		b := &backends[i]
		consider(b, StorageCeiling(b), time.Time{})
		for _, u := range b.UsageLimits {
			consider(b, UsageCeiling(u), b.UsageCycle.NextReset)
		}
	}
	return nearest, found
}

// ResetText renders how long until the limit resets, as "resets in 4d", and
// nothing for a limit that never resets.
func (n *BindingLimit) ResetText() string {
	if n.Reset.IsZero() {
		return ""
	}
	return "resets in " + humanize.Duration(max(time.Until(n.Reset), 0))
}

// Cycle names the usage cycle a backend's usage figures are counted in, as
// its first and last day and its timezone.
func Cycle(c adminapi.UsageCycle) string {
	return humanize.Cycle(c.Start, c.NextReset, c.Timezone)
}
