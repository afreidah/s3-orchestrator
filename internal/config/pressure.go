// -------------------------------------------------------------------------------
// Backend Pressure Configuration
//
// Author: Alex Freidah
//
// Defines BackendPressureConfig, which turns on demoting a backend whose calls
// have slowed against its own baseline, and PressurePolicy, the figures a
// backend is judged by. On by default, since pressure only reorders backends
// and never refuses one.
// -------------------------------------------------------------------------------

package config

import "cmp"

// BackendPressureConfig turns pressure on or off and holds the policy every
// backend is judged by.
type BackendPressureConfig struct {
	Enabled        *bool `yaml:"enabled"` // default true
	PressurePolicy `yaml:",inline"`
}

// PressurePolicy is what a backend's calls are judged by. A call is slow past
// SlowFactor times the backend's baseline. The backend is under pressure once
// SlowShare of its calls are slow or TimeoutShare of them hit the backend
// timeout, judged only on at least MinCalls calls. A zero field is unset.
type PressurePolicy struct {
	SlowFactor   float64 `yaml:"slow_factor"`   // default 3
	SlowShare    float64 `yaml:"slow_share"`    // default 0.25
	TimeoutShare float64 `yaml:"timeout_share"` // default 0.05
	MinCalls     int     `yaml:"min_calls"`     // default 20
}

// DefaultPressurePolicy is the policy a backend is judged by where the config
// sets none of it.
func DefaultPressurePolicy() PressurePolicy {
	return PressurePolicy{SlowFactor: 3, SlowShare: 0.25, TimeoutShare: 0.05, MinCalls: 20}
}

// IsEnabled reports whether backends are judged for pressure, which they are
// unless the operator turned it off.
func (p *BackendPressureConfig) IsEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}

// setDefaultsAndValidate fills the policy's defaults and refuses a share
// outside 0 to 1, a factor that is not above 1, or no minimum.
func (p *BackendPressureConfig) setDefaultsAndValidate() []error {
	def := DefaultPressurePolicy()
	p.Enabled = new(p.IsEnabled())
	p.SlowFactor = cmp.Or(p.SlowFactor, def.SlowFactor)
	p.SlowShare = cmp.Or(p.SlowShare, def.SlowShare)
	p.TimeoutShare = cmp.Or(p.TimeoutShare, def.TimeoutShare)
	p.MinCalls = cmp.Or(p.MinCalls, def.MinCalls)

	errs := failed(
		between("backend_pressure.slow_share", p.SlowShare, 0, 1),
		between("backend_pressure.timeout_share", p.TimeoutShare, 0, 1),
		positive("backend_pressure.min_calls", p.MinCalls),
	)
	if p.SlowFactor <= 1 {
		errs = append(errs, ErrPressureSlowFactor)
	}
	return errs
}
