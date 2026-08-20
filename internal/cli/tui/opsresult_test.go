// -------------------------------------------------------------------------------
// TUI - One-Shot Action Result Tests
//
// Author: Alex Freidah
//
// Each case feeds the JSON its endpoint actually returns, so a change to an
// adminapi response shape breaks these rather than silently degrading the line
// the ops pane prints.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// TestDecodeOneShot renders each one-shot action's response, covering the pass
// that did work, the pass that had nothing to do, and the pass that skipped.
func TestDecodeOneShot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		decode  oneShotDecoder
		body    string
		outcome string
		message string
	}{
		{
			name:    "usage reconcile skipped",
			decode:  decodeOneShot[usageReconcileResult],
			body:    `{"status":"skipped"}`,
			outcome: adminstream.OutcomeSkipped,
			message: "skipped",
		},
		{
			name:    "usage reconcile corrected counters",
			decode:  decodeOneShot[usageReconcileResult],
			body:    `{"status":"ok","adjustments":{"oci":4300000,"e2":-170000000}}`,
			outcome: adminstream.OutcomeOK,
			message: "corrected 2 backends: e2 -162.1 MiB, oci +4.1 MiB",
		},
		{
			name:    "usage reconcile found nothing to correct",
			decode:  decodeOneShot[usageReconcileResult],
			body:    `{"status":"ok","adjustments":{}}`,
			outcome: adminstream.OutcomeOK,
			message: "counters already accurate",
		},
		{
			name:    "cache flush dropped one entry",
			decode:  decodeOneShot[cacheFlushResult],
			body:    `{"status":"flushed","entries_dropped":1}`,
			outcome: adminstream.OutcomeOK,
			message: "dropped 1 cache entry",
		},
		{
			name:    "usage reconcile corrected one backend",
			decode:  decodeOneShot[usageReconcileResult],
			body:    `{"status":"ok","adjustments":{"oci":-1024}}`,
			outcome: adminstream.OutcomeOK,
			message: "corrected 1 backend: oci -1.0 KiB",
		},
		{
			name:    "cache flush dropped entries",
			decode:  decodeOneShot[cacheFlushResult],
			body:    `{"status":"flushed","entries_dropped":1204}`,
			outcome: adminstream.OutcomeOK,
			message: "dropped 1,204 cache entries",
		},
		{
			name:    "cache flush found an empty cache",
			decode:  decodeOneShot[cacheFlushResult],
			body:    `{"status":"flushed","entries_dropped":0}`,
			outcome: adminstream.OutcomeOK,
			message: "cache was already empty",
		},
		{
			// The cache endpoints carry no reason, so the status is the message.
			name:    "cache flush on a disabled cache",
			decode:  decodeOneShot[cacheFlushResult],
			body:    `{"status":"disabled","entries_dropped":0}`,
			outcome: adminstream.OutcomeSkipped,
			message: "disabled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, err := tc.decode(strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if e.Kind != adminstream.KindResult {
				t.Errorf("kind = %q, want %q", e.Kind, adminstream.KindResult)
			}
			if e.Outcome != tc.outcome {
				t.Errorf("outcome = %q, want %q", e.Outcome, tc.outcome)
			}
			if e.Message != tc.message {
				t.Errorf("message = %q, want %q", e.Message, tc.message)
			}
		})
	}
}

// TestDecodeOneShot_BadJSON surfaces a malformed body as an error rather than
// an empty result line.
func TestDecodeOneShot_BadJSON(t *testing.T) {
	t.Parallel()
	if _, err := decodeOneShot[cacheFlushResult](strings.NewReader("{")); err == nil {
		t.Error("expected a decode error")
	}
}

// TestGrouped covers the digit boundaries where separator placement changes.
func TestGrouped(t *testing.T) {
	t.Parallel()
	cases := map[int]string{0: "0", 999: "999", 1000: "1,000", 12345: "12,345", 1234567: "1,234,567", -4321: "-4,321"}
	for n, want := range cases {
		if got := grouped(n); got != want {
			t.Errorf("grouped(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestSignedSize asserts a byte delta reads as a direction.
func TestSignedSize(t *testing.T) {
	t.Parallel()
	cases := map[int64]string{0: "+0 B", 2048: "+2.0 KiB", -2048: "-2.0 KiB"}
	for delta, want := range cases {
		if got := signedSize(delta); got != want {
			t.Errorf("signedSize(%d) = %q, want %q", delta, got, want)
		}
	}
}

// TestOpsActions_EveryActionRenders asserts each menu entry either streams its
// own progress or can decode a result, so none falls through to a blank line.
func TestOpsActions_EveryActionRenders(t *testing.T) {
	t.Parallel()
	streaming := 0
	for _, a := range opsActions() {
		if a.result == nil {
			streaming++
			continue
		}
		if _, err := a.result(strings.NewReader(`{"status":"ok"}`)); err != nil {
			t.Errorf("%s: decode: %v", a.label, err)
		}
	}
	if streaming == 0 {
		t.Error("no streaming actions left in the menu")
	}
}

// TestRewriteSummary_WordsThePass asserts a fleet-wide rewrite reports what it
// achieved, and never reads as clean when objects were left behind.
func TestRewriteSummary_WordsThePass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                     string
		verb                     string
		succeeded, failed, total int
		want                     string
	}{
		{"nothing to do", "encrypted", 0, 0, 0, "nothing to encrypt"},
		{"all succeeded", "encrypted", 1200, 0, 1200, "encrypted 1,200 objects"},
		{"partial", "decrypted", 900, 100, 1000, "decrypted 900 objects, 100 failed"},
		{"single object", "rotated", 1, 0, 1, "rotated 1 object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := rewriteSummary(tc.verb, tc.succeeded, tc.failed, tc.total); got != tc.want {
				t.Errorf("rewriteSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNewOneShotResults_Describe pins what each newly wired action reports back
// to the operator.
func TestNewOneShotResults_Describe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		res  opsResult
		want string
	}{
		{
			name: "cache key",
			res: cacheInvalidateKeyResult{adminapi.CacheInvalidateKeyResponse{
				Status: "invalidated", Key: "bucket/photos/cat.jpg",
			}},
			want: "invalidated bucket/photos/cat.jpg",
		},
		{
			name: "usage flush",
			res:  usageFlushResult{adminapi.UsageFlushResponse{Status: "ok"}},
			want: "counters flushed to the database",
		},
		{
			name: "encrypt existing",
			res: encryptExistingResult{adminapi.EncryptExistingResponse{
				Status: "complete", Total: 3,
				Encrypted: 3,
			}},
			want: "encrypted 3 objects",
		},
		{
			name: "decrypt existing",
			res: decryptExistingResult{adminapi.DecryptExistingResponse{
				Status: "complete", Total: 2, Failed: 1,
				Decrypted: 1,
			}},
			want: "decrypted 1 object, 1 failed",
		},
		{
			name: "rotate key",
			res: rotateKeyResult{adminapi.RotateEncryptionKeyResponse{
				Status: "complete", Total: 5,
				Rotated: 5,
			}},
			want: "rotated 5 objects",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.res.describe(); got != tc.want {
				t.Errorf("describe = %q, want %q", got, tc.want)
			}
			if reason := tc.res.skipReason(); reason != "" {
				t.Errorf("skipReason = %q, want none for a completed pass", reason)
			}
		})
	}
}

// TestNewOneShotResults_ReportSkips asserts a pass that did not run says so
// rather than reporting zero work as success.
func TestNewOneShotResults_ReportSkips(t *testing.T) {
	t.Parallel()
	res := encryptExistingResult{adminapi.EncryptExistingResponse{
		Status: statusSkipped,
	}}
	if got := res.skipReason(); got != statusSkipped {
		t.Errorf("skipReason = %q, want %q", got, statusSkipped)
	}
}
