// -------------------------------------------------------------------------------
// Reload Coordinator - Types and Hook Contract
//
// Author: Alex Freidah
//
// Surfaces the structured contract every reloadable subsystem implements
// (Hook), the per-hook outcome record (HookOutcome), and the aggregate
// reload result (Result) operators consume through the admin API.
// Status enums are stable strings so reload status can be exported to
// metrics, logged, and JSON-rendered without further translation.
// -------------------------------------------------------------------------------

package reload

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// HOOK OUTCOMES
// -------------------------------------------------------------------------

// HookStatus describes the outcome of a single hook's Apply call.
type HookStatus string

// HookStatus values. Skipped covers "not configured / not registered"
// (e.g. UI handler when UI is disabled); Failed only fires when Apply
// returned a non-nil error.
const (
	HookApplied HookStatus = "applied"
	HookSkipped HookStatus = "skipped"
	HookFailed  HookStatus = "failed"
)

// HookOutcome captures one hook's contribution to a reload result.
//
// Error is rendered rather than error-typed so the result stays
// JSON-serialisable for the admin reload-status endpoint.
type HookOutcome struct {
	Name   string     `json:"name"`
	Status HookStatus `json:"status"`
	Error  string     `json:"error,omitempty"` // set only when Status is Failed
}

// -------------------------------------------------------------------------
// PASS RESULT
// -------------------------------------------------------------------------

// Status describes the overall outcome of a reload pass.
type Status string

// Status values:
//   - FullSuccess: every hook returned Applied or Skipped.
//   - PartialApplied: at least one hook returned Failed, but the Check
//     pass succeeded so the config was still swapped in.
//   - ValidationFailed: at least one hook's Check returned an error;
//     no Apply ran, no mutation happened, generation unchanged.
//   - LoadFailed: the YAML file failed to load or validate; no
//     hooks ran, generation unchanged.
const (
	ReloadFullSuccess      Status = "full_success"
	ReloadPartialApplied   Status = "partial_applied"
	ReloadValidationFailed Status = "validation_failed"
	ReloadLoadFailed       Status = "load_failed"
)

// Result is the aggregate report from a single reload pass. The
// coordinator stores the most recent result atomically; the admin API
// exposes it for operator inspection. Generation is monotonic and only
// advances on a successful Apply pass (FullSuccess or PartialApplied).
//
// Outcomes lists every hook the coordinator considered, in apply order, skipped
// ones included, so an operator can see which subsystems took no part.
// RequiresRestart is present whatever the status, because a field that needs a
// restart needs it whether or not this pass succeeded.
type Result struct {
	Generation      int64         `json:"generation"`
	Status          Status        `json:"status"`
	Outcomes        []HookOutcome `json:"outcomes"`
	RequiresRestart []string      `json:"requires_restart,omitempty"` // non-reloadable fields that changed
	LoadError       string        `json:"load_error,omitempty"`       // set only when Status is LoadFailed
	StartedAt       time.Time     `json:"started_at"`
	EndedAt         time.Time     `json:"ended_at"`
}

// -------------------------------------------------------------------------
// HOOK CONTRACT
// -------------------------------------------------------------------------

// Hook is the contract every reloadable subsystem implements. The
// coordinator runs Check on every hook first; any error aborts the
// pass before mutation. Apply then runs every hook, collecting per-hook
// outcomes. Apply errors mark the hook failed but do not abort the
// remaining hooks.
//
// Name is a short stable identifier used in logs and outcomes. Check validates
// the new config against the old without mutating subsystem state. Apply
// mutates the live subsystem and returns Applied or Skipped; a non-nil error
// marks the hook Failed whatever status it returned alongside it.
type Hook interface {
	Name() string
	Check(oldCfg, newCfg *config.Config) error
	Apply(ctx context.Context, oldCfg, newCfg *config.Config) (HookStatus, error)
}
