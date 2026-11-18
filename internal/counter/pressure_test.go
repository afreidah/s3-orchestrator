// -------------------------------------------------------------------------------
// Pressure - Tests
//
// Author: Alex Freidah
//
// The pressure vocabulary and how a set renders.
// -------------------------------------------------------------------------------

package counter

import (
	"slices"
	"testing"
)

// TestPressure_BitsAreDistinct pins that every reason occupies its own bit.
func TestPressure_BitsAreDistinct(t *testing.T) {
	t.Parallel()
	if err := pressureNames.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestPressure_Render covers Has, the names and the joined form.
func TestPressure_Render(t *testing.T) {
	t.Parallel()
	both := PressureSlow | PressureTimeouts
	if !both.Has(PressureTimeouts) || PressureSlow.Has(PressureTimeouts) {
		t.Errorf("Has on %s is wrong", both)
	}
	if got := both.Names(); !slices.Equal(got, []string{"slow", "timeouts"}) {
		t.Errorf("Names = %v", got)
	}
	if got := both.String(); got != "slow,timeouts" {
		t.Errorf("String = %q", got)
	}
}
