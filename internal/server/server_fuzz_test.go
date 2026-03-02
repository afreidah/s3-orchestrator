package server

import "testing"

func FuzzIsValidRequestID(f *testing.F) {
	f.Add("abcdef1234567890")
	f.Add("")
	f.Add("ABCDEF")
	f.Add("not-hex!")
	f.Add("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") // 65 chars
	f.Add("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")     // 64 chars

	f.Fuzz(func(t *testing.T, id string) {
		result := isValidRequestID(id)

		// If valid, verify our invariants hold.
		if result {
			if len(id) == 0 || len(id) > 64 {
				t.Errorf("isValidRequestID(%q) = true but length %d is out of range", id, len(id))
			}
			for _, c := range id {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
					t.Errorf("isValidRequestID(%q) = true but contains non-hex char %q", id, c)
				}
			}
		}
	})
}
