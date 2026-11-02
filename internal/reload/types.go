// -------------------------------------------------------------------------------
// Reload Coordinator - Types and Hook Contract
//
// Author: Alex Freidah
//
// Surfaces the structured contract every reloadable subsystem implements
// (Hook), the per-hook outcome record (HookOutcome), and the aggregate
// reload result (Result), which the runtime converts to the admin API's
// reload status. Status enums are stable strings so reload status can be
// logged and published without further translation.
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

// HookOutcome captures one hook's contribution to a reload result. Error is
// the rendered failure, set only when Status is Failed.
type HookOutcome struct {
	Name   string
	Status HookStatus
	Error  string
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

// Result is the aggregate report from a single reload pass. Generation
// advances only on FullSuccess or PartialApplied. Outcomes lists every hook
// considered, in apply order, skipped ones included. RequiresRestart, the
// non-reloadable fields changed since startup, is reported whatever the
// status; LoadError is set only when Status is LoadFailed.
type Result struct {
	Generation      int64
	Status          Status
	Outcomes        []HookOutcome
	RequiresRestart []string
	LoadError       string
	StartedAt       time.Time
	EndedAt         time.Time
}

// -------------------------------------------------------------------------
// HOOK CONTRACT
// -------------------------------------------------------------------------

// Hook is the contract every reloadable subsystem implements. Apply runs on
// every hook with the new config; a non-nil error marks that hook Failed
// whatever status it returned, without aborting the rest.
type Hook interface {
	Name() string
	Apply(ctx context.Context, cfg *config.Config) (HookStatus, error)
}

// checker is a Hook that can refuse a config before anything changes. The
// coordinator runs Check on every hook implementing it first, and any error
// aborts the pass before mutation; Check must not mutate state.
type checker interface {
	Check(cfg *config.Config) error
}
