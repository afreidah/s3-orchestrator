// -------------------------------------------------------------------------------
// Pressure - Why a Backend Is Tried Last
//
// Author: Alex Freidah
//
// The reasons a backend is under pressure, as a bit per reason. A backend under
// any pressure is still served, but tried after the others: last among a read's
// copies and last among a write's targets.
//
// The set lives in memory per instance and is never stored, so bits may be
// renumbered.
// -------------------------------------------------------------------------------

package counter

import "github.com/afreidah/s3-orchestrator/internal/util/bitset"

// Pressure is the set of reasons a backend is under pressure. The empty set
// means it is not.
type Pressure uint64

// The pressure reasons. PressureSlow is too many calls slower than the
// backend's baseline. PressureTimeouts is too many calls ended by the
// orchestrator's own backend timeout.
const (
	PressureSlow Pressure = 1 << iota
	PressureTimeouts
)

// pressureNames pairs each reason with its name, in the order a rendered set
// lists them.
var pressureNames = bitset.Vocabulary[Pressure]{
	{Bit: PressureSlow, Name: "slow"},
	{Bit: PressureTimeouts, Name: "timeouts"},
}

// Has reports whether the set carries every reason in want.
func (p Pressure) Has(want Pressure) bool {
	return bitset.Has(p, want)
}

// Names lists the reasons in the set by name.
func (p Pressure) Names() []string {
	return pressureNames.Names(p)
}

// String renders the set as its reason names joined by commas.
func (p Pressure) String() string {
	return pressureNames.Join(p)
}
