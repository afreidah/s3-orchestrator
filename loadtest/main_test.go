// -------------------------------------------------------------------------------
// S3 Orchestrator - Loadtest pure-function tests
//
// Covers the formatting and parsing helpers in main.go. Anything that
// needs a live endpoint (runScenario, seedObjects, newTargeter) is
// exercised end-to-end by run-suite.sh, not here.
// -------------------------------------------------------------------------------

package main

import (
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
