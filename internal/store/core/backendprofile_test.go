// -------------------------------------------------------------------------------
// Backend Profile Tests
//
// Author: Alex Freidah
//
// Pins the size-cap rule write placement reads from a backend's profile: a cap
// admits an object up to and including its size, and no cap admits anything.
// -------------------------------------------------------------------------------

package core

import "testing"

// TestBackendProfile_Accepts covers an object under, at and over a cap, and a
// backend with no cap.
func TestBackendProfile_Accepts(t *testing.T) {
	t.Parallel()
	capped := BackendProfile{MaxObjectSize: 100}
	for _, tc := range []struct {
		name    string
		profile BackendProfile
		size    int64
		want    bool
	}{
		{"under the cap", capped, 99, true},
		{"at the cap", capped, 100, true},
		{"over the cap", capped, 101, false},
		{"no cap", BackendProfile{}, 1 << 40, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.profile.Accepts(tc.size); got != tc.want {
				t.Errorf("Accepts(%d) = %v, want %v", tc.size, got, tc.want)
			}
		})
	}
}
