// -------------------------------------------------------------------------------
// Backend State Flags
//
// Author: Alex Freidah
//
// The operator-set state of a backend, as a bit per flag. A backend with no
// flags set is in normal service. Each consumer asks whether a backend's flags
// are all ones it accepts, through the accept mask for its purpose, and the
// masks live here so consumers that want the same backends share one
// definition. A flag added later is refused by every consumer until a mask
// accepts it, so forgetting to update a mask holds a backend back rather than
// sending it work it should not get.
//
// The set is stored as an integer and admission tests it in SQL against a mask
// the store passes in, so bit positions are part of the schema: a flag added
// later takes the next bit, and no existing bit is ever renumbered.
// -------------------------------------------------------------------------------

package core

import (
	"encoding/json"

	"github.com/afreidah/s3-orchestrator/internal/util/bitset"
)

// BackendFlags is a backend's operator-set state. Flags are combined with OR
// when set, and tested against an accept mask. It is signed to match the
// database integer column it is stored in.
type BackendFlags int64

// The backend flags. Cordoned refuses new copies and is independent of a
// drain. Draining, drained and drain-failed are the drain's progress, and at
// most one of them is set at a time.
const (
	FlagCordoned BackendFlags = 1 << iota
	FlagDraining
	FlagDrained
	FlagDrainFailed
)

// DrainFlags is every flag that belongs to a drain: in progress, finished or
// failed.
const DrainFlags = FlagDraining | FlagDrained | FlagDrainFailed

// The accept masks. AcceptForWrite is what a backend may carry and still be
// given new copies - client writes, replicas, rebalance moves and drain
// destinations - which is nothing. AcceptForReconcile lets a cordoned backend
// be reconciled, since only a drain is moving its rows off. AcceptForRewrite
// lets a cordoned backend have a copy it holds rewritten in place, which adds
// no copy to it.
const (
	AcceptForWrite     BackendFlags = 0
	AcceptForReconcile              = FlagCordoned
	AcceptForRewrite                = FlagCordoned
)

// backendFlagNames pairs each flag with its name, in the order a rendered set
// lists them.
var backendFlagNames = bitset.Vocabulary[BackendFlags]{
	{Bit: FlagCordoned, Name: "cordoned"},
	{Bit: FlagDraining, Name: "draining"},
	{Bit: FlagDrained, Name: "drained"},
	{Bit: FlagDrainFailed, Name: "drain-failed"},
}

// The drain-progress names DrainState reports, which the drain-progress API
// carries.
const (
	drainStateDraining = "draining"
	drainStateDrained  = "drained"
	drainStateFailed   = "failed"
)

// AcceptedBy reports whether every flag in the set is one accept allows. A
// backend with no flags is accepted by any mask.
func (f BackendFlags) AcceptedBy(accept BackendFlags) bool {
	return bitset.Within(f, accept)
}

// Any reports whether the set carries at least one flag in mask.
func (f BackendFlags) Any(mask BackendFlags) bool {
	return bitset.Any(f, mask)
}

// Names lists the flags in the set by name.
func (f BackendFlags) Names() []string {
	return backendFlagNames.Names(f)
}

// String renders the set as its flag names joined by commas.
func (f BackendFlags) String() string {
	return backendFlagNames.Join(f)
}

// MarshalJSON renders the set as its flag names, so an API carrying it reads
// ["cordoned"] rather than a bit number.
func (f BackendFlags) MarshalJSON() ([]byte, error) {
	return json.Marshal(f.Names())
}

// DrainState names where the backend's drain stands: draining, drained or
// failed, or empty when it carries no drain flag.
func (f BackendFlags) DrainState() string {
	switch {
	case f.Any(FlagDraining):
		return drainStateDraining
	case f.Any(FlagDrained):
		return drainStateDrained
	case f.Any(FlagDrainFailed):
		return drainStateFailed
	}
	return ""
}
