// -------------------------------------------------------------------------------
// Humanize Tests - Byte Size Formatting
//
// Author: Alex Freidah
//
// Pins the contract the four previous copies of this formatter disagreed on:
// what a zero and a negative count render as, and where each IEC step begins.
// -------------------------------------------------------------------------------

package humanize

import "testing"

func TestBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   int64
		want string
	}{
		{"zero", 0, "0 B"},
		{"bytes", 512, "512 B"},
		{"one below the step", 1023, "1023 B"},
		{"exact step", 1024, "1.0 KiB"},
		{"kib", 2048, "2.0 KiB"},
		{"mib", 5 * 1024 * 1024, "5.0 MiB"},
		{"gib", 10 * 1024 * 1024 * 1024, "10.0 GiB"},
		{"tib", 3 * 1024 * 1024 * 1024 * 1024, "3.0 TiB"},
		{"rounds to one decimal", 1536, "1.5 KiB"},
		{"rounds down", 2516582, "2.4 MiB"},
		// Negatives keep their sign: a negative byte count is a delta or a
		// drifted counter, and clamping it to zero hides that.
		{"small negative", -512, "-512 B"},
		{"negative kib", -5000, "-4.9 KiB"},
		{"negative exact step", -1024, "-1.0 KiB"},
		{"negative mib", -5 * 1024 * 1024, "-5.0 MiB"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := Bytes(c.in); got != c.want {
				t.Errorf("Bytes(%d) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestBytes_LargestUnitDoesNotOverflowThePrefixTable guards the loop bound:
// running past "E" would panic on the prefix index rather than misformat.
func TestBytes_LargestUnitDoesNotOverflowThePrefixTable(t *testing.T) {
	t.Parallel()
	for _, n := range []int64{1<<62 - 1, -(1 << 62)} {
		if Bytes(n) == "" {
			t.Errorf("Bytes(%d) returned empty", n)
		}
	}
}
