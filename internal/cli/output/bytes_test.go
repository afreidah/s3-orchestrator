// -------------------------------------------------------------------------------
// CLI Output - Byte Size Formatting Tests
//
// Author: Alex Freidah
//
// Covers the IEC unit boundaries of FormatBytes from plain bytes through TiB.
// -------------------------------------------------------------------------------

package output

import "testing"

func TestFormatBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{10737418240, "10.0 GiB"},
		{1099511627776, "1.0 TiB"},
	}
	for _, tc := range tests {
		if got := FormatBytes(tc.n); got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
