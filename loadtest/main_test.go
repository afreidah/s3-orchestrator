// -------------------------------------------------------------------------------
// S3 Orchestrator - Loadtest pure-function tests
//
// Covers the formatting and parsing helpers in main.go. Anything that
// needs a live endpoint (runScenario, seedObjects, newTargeter) is
// exercised end-to-end by run-suite.sh, not here.
// -------------------------------------------------------------------------------

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"
)

func TestParseSizes_SingleFallback(t *testing.T) {
	got, err := parseSizes("", 4096)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != 4096 {
		t.Fatalf("got %v, want [4096]", got)
	}
}

func TestParseSizes_CSV(t *testing.T) {
	got, err := parseSizes("1024, 65536 , 1048576", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{1024, 65536, 1048576}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("idx %d: got %d, want %d", i, got[i], want[i])
		}
	}
}

func TestParseSizes_InvalidSingle(t *testing.T) {
	if _, err := parseSizes("", 0); err == nil {
		t.Fatal("expected error for size=0")
	}
	if _, err := parseSizes("", -1); err == nil {
		t.Fatal("expected error for size=-1")
	}
}

func TestParseSizes_InvalidCSV(t *testing.T) {
	if _, err := parseSizes("abc", 0); err == nil {
		t.Fatal("expected error for non-numeric size")
	}
	if _, err := parseSizes("0", 0); err == nil {
		t.Fatal("expected error for zero in CSV")
	}
	if _, err := parseSizes("-5", 0); err == nil {
		t.Fatal("expected error for negative in CSV")
	}
}

func TestParseSizes_EmptyAfterTrim(t *testing.T) {
	if _, err := parseSizes(" , , ", 0); err == nil {
		t.Fatal("expected error for csv that trims to empty")
	}
}

func TestNewHardwareInfo_Populated(t *testing.T) {
	h := newHardwareInfo()
	if h.OS == "" || h.Arch == "" || h.GoVersion == "" || h.NumCPU == 0 {
		t.Fatalf("hardwareInfo missing fields: %+v", h)
	}
}

func TestSummarise_PopulatesMatrix(t *testing.T) {
	m := &vegeta.Metrics{}
	m.StatusCodes = map[string]int{"200": 10, "503": 2}
	m.Requests = 12
	m.Duration = 10 * time.Second
	m.Rate = 1.2
	m.Throughput = 1.0
	m.Success = 10.0 / 12.0
	m.Latencies.P50 = 5 * time.Millisecond
	m.Latencies.P95 = 50 * time.Millisecond
	m.Latencies.P99 = 100 * time.Millisecond
	m.Latencies.Max = 200 * time.Millisecond

	got := summarise(1024, 100, m)

	if got.SizeBytes != 1024 || got.RequestedRPS != 100 {
		t.Errorf("size/rate mismatch: %+v", got)
	}
	if got.P50Ms != 5 || got.P95Ms != 50 || got.P99Ms != 100 || got.MaxMs != 200 {
		t.Errorf("latency conversion wrong: %+v", got)
	}
	wantErr := 1.0 - (10.0 / 12.0)
	if diff := got.ErrorRate - wantErr; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("error rate = %v, want %v", got.ErrorRate, wantErr)
	}
	if got.StatusCodes["200"] != 10 || got.StatusCodes["503"] != 2 {
		t.Errorf("status codes not copied: %+v", got.StatusCodes)
	}
	if got.BytesPerSec != 1024.0 {
		t.Errorf("bytes_per_sec = %v, want 1024", got.BytesPerSec)
	}
}

func TestPrintMarkdownSummary_RampMode(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "summary-*.md")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	r := &sweepResults{
		Scenario:      "put",
		Mode:          "ramp",
		Duration:      "30s",
		Workers:       10,
		SaturationRPS: 1000,
		Results: []runResult{
			{RequestedRPS: 500, ThroughputRPS: 500.1, Requests: 15000, P50Ms: 1, P95Ms: 5, P99Ms: 10, MaxMs: 50, ErrorRate: 0.0},
			{RequestedRPS: 1000, ThroughputRPS: 998.7, Requests: 30000, P50Ms: 2, P95Ms: 20, P99Ms: 100, MaxMs: 500, ErrorRate: 0.08},
		},
	}
	printMarkdownSummary(tmp, r)
	tmp.Close()

	out, _ := os.ReadFile(tmp.Name())
	body := string(out)
	if !strings.Contains(body, "ramp mode") {
		t.Errorf("missing ramp mode header: %s", body)
	}
	if !strings.Contains(body, "Saturation point:** 1000 req/s") {
		t.Errorf("missing saturation marker: %s", body)
	}
	if !strings.Contains(body, "Requested RPS") {
		t.Errorf("missing ramp table header: %s", body)
	}
}

func TestPrintMarkdownSummary_SizeSweep(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "summary-*.md")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	r := &sweepResults{
		Scenario: "put",
		Mode:     "size-sweep",
		Duration: "30s",
		Workers:  10,
		Results: []runResult{
			{SizeBytes: 1024, Requests: 3000, ThroughputRPS: 100.0, BytesPerSec: 1024 * 100, P50Ms: 1, P95Ms: 5, P99Ms: 10, MaxMs: 50, ErrorRate: 0},
		},
	}
	printMarkdownSummary(tmp, r)
	tmp.Close()

	out, _ := os.ReadFile(tmp.Name())
	body := string(out)
	if !strings.Contains(body, "size-sweep mode") {
		t.Errorf("missing mode header: %s", body)
	}
	if !strings.Contains(body, "Size (B)") {
		t.Errorf("missing size-sweep header: %s", body)
	}
	if strings.Contains(body, "Saturation point") {
		t.Errorf("size-sweep summary should not contain saturation marker: %s", body)
	}
}

func TestWriteJSON_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.json")
	r := &sweepResults{
		Scenario:  "put",
		Endpoint:  "http://localhost:9000",
		Bucket:    "photos",
		Rate:      500,
		Duration:  "30s",
		Workers:   10,
		Mode:      "ramp",
		Hardware:  newHardwareInfo(),
		StartedAt: time.Now().UTC(),
		Results: []runResult{
			{SizeBytes: 1024, RequestedRPS: 500, ThroughputRPS: 499.9, ErrorRate: 0.0},
		},
	}
	if err := writeJSON(path, r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var got sweepResults
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("re-decode failed: %v", err)
	}
	if got.Scenario != "put" || got.Mode != "ramp" || len(got.Results) != 1 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Hardware.OS == "" {
		t.Error("hardware fingerprint lost in round-trip")
	}
}

// TestEnforceErrorBudget_FailsAboveBudget verifies a run that completed while
// failing a large share of its requests is reported as a failure. This is the
// gate that was missing: a scenario used to pass purely because the binary ran
// to completion.
func TestEnforceErrorBudget_FailsAboveBudget(t *testing.T) {
	results := &sweepResults{Results: []runResult{
		{SizeBytes: 1024, RequestedRPS: 200, ErrorRate: 0.0},
		{SizeBytes: 65536, RequestedRPS: 200, ErrorRate: 0.2699},
	}}
	err := enforceErrorBudget(results, 0.01, false)
	if err == nil {
		t.Fatal("26.99% errors against a 1% budget must fail")
	}
	// The message has to name the offending step and both numbers, or the
	// summary tells an operator nothing about which size blew the budget.
	for _, want := range []string{"65536", "26.99", "1.00"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// TestEnforceErrorBudget_PassesWithinBudget verifies a clean run is unaffected.
func TestEnforceErrorBudget_PassesWithinBudget(t *testing.T) {
	results := &sweepResults{Results: []runResult{
		{SizeBytes: 1024, ErrorRate: 0.0},
		{SizeBytes: 65536, ErrorRate: 0.005},
	}}
	if err := enforceErrorBudget(results, 0.01, false); err != nil {
		t.Errorf("a run inside its budget must pass, got %v", err)
	}
}

// TestEnforceErrorBudget_ZeroDisables verifies the budget can be turned off,
// which is how ad-hoc measurement runs opt out of the gate.
func TestEnforceErrorBudget_ZeroDisables(t *testing.T) {
	results := &sweepResults{Results: []runResult{{ErrorRate: 1.0}}}
	if err := enforceErrorBudget(results, 0, false); err != nil {
		t.Errorf("a zero budget disables the gate, got %v", err)
	}
}

// TestEnforceErrorBudget_RampExempt verifies a ramp run is never failed by the
// budget. Ramps drive the system into saturation on purpose, so a high error
// rate at the top of the ramp is the measurement, not a fault.
func TestEnforceErrorBudget_RampExempt(t *testing.T) {
	results := &sweepResults{Results: []runResult{
		{RequestedRPS: 4800, ErrorRate: 0.42},
	}}
	if err := enforceErrorBudget(results, 0.01, true); err != nil {
		t.Errorf("a ramp run must be exempt from the budget, got %v", err)
	}
}

// TestEnforceErrorBudget_BoundaryIsInclusive verifies a run sitting exactly on
// its budget passes, so the threshold reads as "at most this much".
func TestEnforceErrorBudget_BoundaryIsInclusive(t *testing.T) {
	results := &sweepResults{Results: []runResult{{ErrorRate: 0.01}}}
	if err := enforceErrorBudget(results, 0.01, false); err != nil {
		t.Errorf("exactly at budget must pass, got %v", err)
	}
}

// TestNeedsSeeding covers which operations require a pre-existing working set.
// The tagging scenario tags objects that already exist, so it seeds; the
// inline-tagging PUT creates its own objects and does not.
func TestNeedsSeeding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		op   string
		want bool
	}{
		{"put", false},
		{"puttagged", false},
		{"get", true},
		{"mixed", true},
		{"listobjects", true},
		{"tagging", true},
	}
	for _, tc := range tests {
		if got := needsSeeding(tc.op); got != tc.want {
			t.Errorf("needsSeeding(%q) = %v, want %v", tc.op, got, tc.want)
		}
	}
}

// TestNewBody_RandomByDefault verifies the default payload stays random, since
// that is what measures the write path without an encoder shortening it.
func TestNewBody_RandomByDefault(t *testing.T) {
	body, err := newBody(4096, 0)
	if err != nil {
		t.Fatalf("newBody: %v", err)
	}
	if len(body) != 4096 {
		t.Fatalf("body is %d bytes, want 4096", len(body))
	}
	if compressedFraction(t, body) < 0.9 {
		t.Error("the default body compressed, so it was not random")
	}
}

// TestNewBody_CompressibleShrinks verifies a body asked to be compressible
// actually encodes smaller. Without it the suite measures compression only as
// the cost of declining, never as the work of encoding.
func TestNewBody_CompressibleShrinks(t *testing.T) {
	body, err := newBody(64<<10, 0.8)
	if err != nil {
		t.Fatalf("newBody: %v", err)
	}
	if len(body) != 64<<10 {
		t.Fatalf("body is %d bytes, want %d", len(body), 64<<10)
	}
	if got := compressedFraction(t, body); got > 0.5 {
		t.Errorf("body encoded to %.2f of its size at compressible=0.8, want well under half", got)
	}
}

// TestNewBody_FullyCompressible verifies the ceiling is handled: a fraction of
// 1 fills the whole body rather than running past the end of it.
func TestNewBody_FullyCompressible(t *testing.T) {
	body, err := newBody(8192, 1)
	if err != nil {
		t.Fatalf("newBody: %v", err)
	}
	if len(body) != 8192 {
		t.Fatalf("body is %d bytes, want 8192", len(body))
	}
	if got := compressedFraction(t, body); got > 0.2 {
		t.Errorf("a fully repetitive body encoded to %.2f of its size", got)
	}
}

// compressedFraction reports what share of its size a body keeps once gzipped,
// which stands in for what the orchestrator's encoder would make of it.
func compressedFraction(t *testing.T, body []byte) float64 {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(body); err != nil {
		t.Fatalf("compress: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close compressor: %v", err)
	}
	return float64(buf.Len()) / float64(len(body))
}
