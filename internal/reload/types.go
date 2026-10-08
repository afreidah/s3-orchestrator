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

// Result is the aggregate report from a single reload pass, exposed by the
// admin API. Generation advances only on FullSuccess or PartialApplied.
// Outcomes lists every hook considered, in apply order, skipped ones included.
// RequiresRestart is reported whatever the status.
type Result struct {
	Generation      int64         `json:"generation"`
	Status          Status        `json:"status"`
	Outcomes        []HookOutcome `json:"outcomes"`
	RequiresRestart []string      `json:"requires_restart,omitempty"` // non-reloadable fields changed since startup
	LoadError       string        `json:"load_error,omitempty"`       // set only when Status is LoadFailed
	StartedAt       time.Time     `json:"started_at"`
	EndedAt         time.Time     `json:"ended_at"`
}

// -------------------------------------------------------------------------
// HOOK CONTRACT
// -------------------------------------------------------------------------

// Hook is the contract every reloadable subsystem implements. The coordinator
// runs Check on every hook first, and any error aborts the pass before
// mutation; Check must not mutate state. Apply then runs every hook; a non-nil
// error marks that hook Failed whatever status it returned, without aborting
// the rest.
type Hook interface {
	Name() string
	Check(oldCfg, newCfg *config.Config) error
	Apply(ctx context.Context, oldCfg, newCfg *config.Config) (HookStatus, error)
}
