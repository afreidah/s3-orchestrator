package auth

import "testing"

func FuzzParseSigV4Fields(f *testing.F) {
	// Valid header fragment.
	f.Add("Credential=AKID/20260215/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abcdef1234567890")
	// Edge cases.
	f.Add("")
	f.Add("no-equals-sign")
	f.Add("Key=Value")
	f.Add("A=1, B=2, C=3")
	f.Add("Dup=first, Dup=second")
	f.Add("=empty-key")
	f.Add("empty-value=")
	f.Add(",,,")
	f.Add("embedded\nnewline=val")

	f.Fuzz(func(t *testing.T, input string) {
		// Must not panic. Return value is always a map.
		result := parseSigV4Fields(input)
		if result == nil {
			t.Error("parseSigV4Fields returned nil")
		}
	})
}
