// -------------------------------------------------------------------------------
// Reload Coordinator - Types and Hook Contract
//
// Author: Alex Freidah
//
// Surfaces the structured contract every reloadable subsystem implements
// (Hook), the per-hook outcome record (HookOutcome), and the aggregate
// reload result (ReloadResult) operators consume through the admin API.
// Status enums are stable strings so reload status can be exported to
// metrics, logged, and JSON-rendered without further translation.
// -------------------------------------------------------------------------------

package reload

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

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
type HookOutcome struct {
	Name   string     `json:"name"`
	Status HookStatus `json:"status"`
	// Error is the rendered error message when Status is Failed.
	// Empty otherwise. Rendered (not error-typed) so the result is
	// JSON-serialisable for the admin reload-status endpoint.
	Error string `json:"error,omitempty"`
}

// ReloadStatus describes the overall outcome of a reload pass.
type ReloadStatus string

// ReloadStatus values:
//   - FullSuccess: every hook returned Applied or Skipped.
//   - PartialApplied: at least one hook returned Failed, but the Check
//     pass succeeded so the config was still swapped in.
//   - ValidationFailed: at least one hook's Check returned an error;
//     no Apply ran, no mutation happened, generation unchanged.
//   - LoadFailed: the YAML file failed to load or validate; no
//     hooks ran, generation unchanged.
const (
	ReloadFullSuccess      ReloadStatus = "full_success"
	ReloadPartialApplied   ReloadStatus = "partial_applied"
	ReloadValidationFailed ReloadStatus = "validation_failed"
	ReloadLoadFailed       ReloadStatus = "load_failed"
)

// ReloadResult is the aggregate report from a single reload pass. The
// coordinator stores the most recent result atomically; the admin API
// exposes it for operator inspection. Generation is monotonic and only
// advances on a successful Apply pass (FullSuccess or PartialApplied).
type ReloadResult struct {
	Generation int64        `json:"generation"`
	Status     ReloadStatus `json:"status"`
	// Outcomes lists every hook the coordinator considered, in apply
	// order. Skipped hooks are recorded too so operators can see which
	// subsystems were not part of the reload.
	Outcomes []HookOutcome `json:"outcomes"`
	// RequiresRestart lists non-reloadable config fields whose value
	// changed since the last successful load. Present regardless of
	// Status so operators always see the warning.
	RequiresRestart []string `json:"requires_restart,omitempty"`
	// LoadError is the rendered LoadConfig error when Status is
	// LoadFailed. Empty otherwise.
	LoadError string `json:"load_error,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// Hook is the contract every reloadable subsystem implements. The
// coordinator runs Check on every hook first; any error aborts the
// pass before mutation. Apply then runs every hook, collecting per-hook
// outcomes. Apply errors mark the hook failed but do not abort the
// remaining hooks.
type Hook interface {
	// Name is a short stable identifier used in logs and outcomes.
	Name() string
	// Check validates the new config against the old one without
	// mutating any subsystem state. Returning an error aborts the
	// reload before any Apply fires.
	Check(oldCfg, newCfg *config.Config) error
	// Apply mutates the live subsystem. Returns HookApplied when the
	// mutation ran, HookSkipped when the subsystem is not configured
	// or not registered. Returning a non-nil error marks the hook
	// Failed regardless of the returned status.
	Apply(ctx context.Context, oldCfg, newCfg *config.Config) (HookStatus, error)
}
