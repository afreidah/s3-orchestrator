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
			name:    "requeue names its backend",
			decode:  decodeOneShot[requeueResult],
			body:    `{"backend":"b2","requeued":4}`,
			outcome: adminstream.OutcomeOK,
			message: "requeued 4 rows from b2",
		},
		{
			name:    "unscoped requeue covers every backend",
			decode:  decodeOneShot[requeueResult],
			body:    `{"requeued":1}`,
			outcome: adminstream.OutcomeOK,
			message: "requeued 1 row from all backends",
		},
		{
			name:    "queue rebuild at the deployment's factor",
			decode:  decodeOneShot[replicationRebuildResult],
			body:    `{"status":"ok","factor":2,"queued":1204}`,
			outcome: adminstream.OutcomeOK,
			message: "queued 1,204 keys off factor 2",
		},
		{
			name:    "queue rebuild with buckets at their own factor",
			decode:  decodeOneShot[replicationRebuildResult],
			body:    `{"status":"ok","factor":2,"bucket_factors":{"wal":1,"traces":1},"queued":3}`,
			outcome: adminstream.OutcomeOK,
			message: "queued 3 keys off factor 2, 2 buckets set their own",
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
	res := usageReconcileResult{adminapi.UsageReconcileResponse{
		Status: adminstream.OutcomeSkipped,
	}}
	if got := res.skipReason(); got != adminstream.OutcomeSkipped {
		t.Errorf("skipReason = %q, want %q", got, adminstream.OutcomeSkipped)
	}
}
