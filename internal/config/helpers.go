// -------------------------------------------------------------------------------
// Config - Validation Helpers
//
// Author: Alex Freidah
//
// Generic helpers used by setDefaultsAndValidate functions across the
// config package. Replace `if x == 0 { x = default }` chains with a
// single expression so validators read top-to-bottom as a list of
// intentions rather than a mix of state machines.
// -------------------------------------------------------------------------------

package config

// defaulted returns def when v is the zero value for T, otherwise v.
// Used for "0 or unset means use the default" config knobs where the
// zero value is not a meaningful operator choice.
func defaulted[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}
