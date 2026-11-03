// -------------------------------------------------------------------------------
// Usage Stat Tests
//
// Author: Alex Freidah
//
// Pins that Plus sums each field on its own.
// -------------------------------------------------------------------------------

package usagestat

import "testing"

// TestStat_Plus checks every field adds to its own counterpart.
func TestStat_Plus(t *testing.T) {
	t.Parallel()
	got := Stat{APIRequests: 1, EgressBytes: 10, IngressBytes: 100}.Plus(Stat{APIRequests: 2, EgressBytes: 20, IngressBytes: 200})
	if want := (Stat{APIRequests: 3, EgressBytes: 30, IngressBytes: 300}); got != want {
		t.Errorf("Plus = %+v, want %+v", got, want)
	}
}
