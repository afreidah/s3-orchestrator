// -------------------------------------------------------------------------------
// S3 Orchestrator Load Tester
//
// Author: Alex Freidah
//
// Vegeta-based load testing tool with SigV4 authentication for the S3 API.
// Supports constant-rate PUT, GET, and mixed workloads against any
// S3-compatible endpoint, plus an object-size sweep mode that runs the
// same scenario at multiple sizes and emits a structured JSON results
// matrix for performance-envelope characterisation.
//
// Usage:
//
//	go run . -op put -rate 200 -duration 30s -size 4096
//	go run . -op get -rate 500 -duration 1m -seed 1000
//	go run . -op mixed -rate 300 -duration 2m -seed 500
//	go run . -op put -rate 200 -duration 30s -sizes 1024,1048576,104857600
//	go run . -op get -rate 200 -duration 30s -sizes 1024,1048576 -output-json results.json
//
// -------------------------------------------------------------------------------
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	vegeta "github.com/tsenart/vegeta/v12/lib"
)

// unsignedPayload tells the server to skip payload hash verification.
// Used for all methods - this is a load tester, not a security tool.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// defaultMaxErrorRate is the share of failed requests a run may absorb before
// it is reported as a failure. Non-zero by default so a scenario added without
// an explicit budget is gated rather than silently unchecked.
const defaultMaxErrorRate = 0.01

// needsSeeding reports whether an operation reads objects, and so requires a
// working set to exist on the endpoint before the run starts.
func needsSeeding(op string) bool {
	return op == "get" || op == "mixed" || op == "listobjects" || op == "tagging"
}

// taggingBody is the Tagging document the ?tagging PUT sends. Fixed rather
// than generated per request: the point of the scenario is the cost of the
// write path, not of building two tags.
const taggingBody = `<Tagging><TagSet>` +
	`<Tag><Key>loadtest</Key><Value>1</Value></Tag>` +
	`<Tag><Key>retain</Key><Value>30d</Value></Tag>` +
	`</TagSet></Tagging>`

// inlineTaggingHeader is the x-amz-tagging value a tagged PUT carries. Query
// string encoded, which is the header's format rather than the XML above.
const inlineTaggingHeader = "loadtest=1&retain=30d"

// scenarioConfig captures the immutable inputs to a single scenario run.
// Extracted so the sweep loop can re-run with only the body size varying.
type scenarioConfig struct {
	endpoint    string
	bucket      string
	region      string
	op          string
	rate        int
	duration    time.Duration
	workers     uint64
	seedCount   int
	cold        bool
	listPrefix  string
	listMaxKeys int
	signer      *v4.Signer
	creds       aws.Credentials
	runID       string
}

// runResult is the per-size summary for a single scenario run, structured
// for JSON emission and direct rendering as a Markdown row.
type runResult struct {
	SizeBytes     int            `json:"size_bytes"`
	RequestedRPS  int            `json:"requested_rps"`
	Requests      uint64         `json:"requests"`
	DurationSec   float64        `json:"duration_sec"`
	ThroughputRPS float64        `json:"throughput_rps"`
	BytesPerSec   float64        `json:"bytes_per_sec"`
	P50Ms         float64        `json:"p50_ms"`
	P95Ms         float64        `json:"p95_ms"`
	P99Ms         float64        `json:"p99_ms"`
	MaxMs         float64        `json:"max_ms"`
	ErrorRate     float64        `json:"error_rate"`
	StatusCodes   map[string]int `json:"status_codes"`
}

// sweepResults is the full output document for one invocation. Contains
// the static scenario inputs, hardware fingerprint, and one runResult
// per object size (single-size runs produce a one-element matrix).
type sweepResults struct {
	Scenario      string       `json:"scenario"`
	Endpoint      string       `json:"endpoint"`
	Bucket        string       `json:"bucket"`
	Rate          int          `json:"rate"`
	Duration      string       `json:"duration"`
	Workers       uint64       `json:"workers"`
	SeedCount     int          `json:"seed_count,omitempty"`
	Mode          string       `json:"mode"`                     // "single", "size-sweep", "ramp"
	SaturationRPS int          `json:"saturation_rps,omitempty"` // ramp mode: rate at which error_rate first exceeded threshold
	Hardware      hardwareInfo `json:"hardware"`
	StartedAt     time.Time    `json:"started_at"`
	Results       []runResult  `json:"results"`
}

// hardwareInfo records the host the loadtest ran on so a results file
// remains interpretable after the fact.
type hardwareInfo struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	NumCPU    int    `json:"num_cpu"`
	GoVersion string `json:"go_version"`
}

// validateRampFlags checks the saturation-find ramp flags. multiSize reports
// whether more than one object size was requested; ramp mode sweeps a single
// dimension, so it is incompatible with a size sweep.
func validateRampFlags(rampTo, rate, rampStep int, multiSize bool) error {
	if rampTo <= 0 {
		return nil
	}
	if multiSize {
		return errors.New("-ramp-to and -sizes are mutually exclusive (one swept dimension at a time)")
	}
	if rampTo <= rate {
		return fmt.Errorf("-ramp-to (%d) must exceed -rate (%d)", rampTo, rate)
	}
	if rampStep <= 0 {
		return errors.New("-ramp-step must be positive")
	}
	return nil
}

// main is the program entry point.
func main() {
	var (
		endpoint         = flag.String("endpoint", "http://localhost:9000", "S3 orchestrator endpoint")
		accessKey        = flag.String("access-key", "photoskey", "Access key ID")
		secretKey        = flag.String("secret-key", "photossecret", "Secret access key")
		bucket           = flag.String("bucket", "photos", "Target bucket")
		region           = flag.String("region", "us-east-1", "AWS region for SigV4")
		rateFlag         = flag.Int("rate", 100, "Requests per second (initial rate when -ramp-to is set)")
		dur              = flag.Duration("duration", 30*time.Second, "Test duration per scenario step")
		size             = flag.Int("size", 1024, "Object size in bytes (ignored if -sizes is set)")
		sizesFlag        = flag.String("sizes", "", "Comma-separated object sizes for sweep mode (e.g. 1024,1048576,104857600); overrides -size")
		op               = flag.String("op", "put", "Operation: put, get, mixed, listobjects, tagging, puttagged")
		workers          = flag.Uint64("workers", 10, "Concurrent workers")
		seedN            = flag.Int("seed", 100, "Objects to pre-seed for get/mixed/listobjects (per size in sweep mode)")
		listPrefix       = flag.String("list-prefix", "loadtest/", "Prefix for listobjects scenario")
		listMaxKeys      = flag.Int("list-max-keys", 1000, "max-keys query parameter for listobjects scenario")
		outputJSON       = flag.String("output-json", "", "Write structured results to this file")
		rampTo           = flag.Int("ramp-to", 0, "Saturation-find: ramp from -rate up to this rate; stop when error rate exceeds -ramp-error-threshold (0 disables ramp mode)")
		rampStep         = flag.Int("ramp-step", 100, "Rate increment per ramp step")
		rampErrThreshold = flag.Float64("ramp-error-threshold", 0.05, "Error rate threshold (0..1) for ramp termination")
		maxErrorRate     = flag.Float64("max-error-rate", defaultMaxErrorRate, "Fail the run when any result exceeds this error rate (0..1); 0 disables")
		cacheFlushBefore = flag.Bool("cache-flush-before", false, "POST /admin/api/cache/flush before each scenario step (requires -admin-token)")
		adminToken       = flag.String("admin-token", "", "Admin token for cache-flush calls (also read from S3O_ADMIN_TOKEN env var)")
		cold             = flag.Bool("cold", false, "Cold-cache read mode for -op get: read each seeded object exactly once so every GET is a first touch; the run lasts one pass over the working set, not -duration")
	)
	flag.Parse()
	if *adminToken == "" {
		*adminToken = os.Getenv("S3O_ADMIN_TOKEN")
	}

	sizes, err := parseSizes(*sizesFlag, *size)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if err := validateRampFlags(*rampTo, *rateFlag, *rampStep, len(sizes) > 1); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if *cacheFlushBefore && *adminToken == "" {
		fmt.Fprintln(os.Stderr, "error: -cache-flush-before requires -admin-token (or S3O_ADMIN_TOKEN env var)")
		os.Exit(1)
	}
	if *cold && *op != "get" {
		fmt.Fprintln(os.Stderr, "error: -cold only applies to -op get")
		os.Exit(1)
	}

	signer := v4.NewSigner()
	creds := aws.Credentials{
		AccessKeyID:     *accessKey,
		SecretAccessKey: *secretKey,
	}

	cfg := scenarioConfig{
		endpoint:    *endpoint,
		bucket:      *bucket,
		region:      *region,
		op:          *op,
		rate:        *rateFlag,
		duration:    *dur,
		workers:     *workers,
		seedCount:   *seedN,
		cold:        *cold,
		listPrefix:  *listPrefix,
		listMaxKeys: *listMaxKeys,
		signer:      signer,
		creds:       creds,
		runID:       time.Now().UTC().Format("20060102T150405Z"),
	}

	results := sweepResults{
		Scenario:  *op,
		Endpoint:  *endpoint,
		Bucket:    *bucket,
		Rate:      *rateFlag,
		Duration:  dur.String(),
		Workers:   *workers,
		Hardware:  newHardwareInfo(),
		StartedAt: time.Now().UTC(),
	}
	if needsSeeding(*op) {
		results.SeedCount = *seedN
	}
	switch {
	case *rampTo > 0:
		results.Mode = "ramp"
	case len(sizes) > 1:
		results.Mode = "size-sweep"
	default:
		results.Mode = "single"
	}

	if *rampTo > 0 {
		runRamp(&cfg, sizes[0], *rampTo, *rampStep, *rampErrThreshold, *cacheFlushBefore, *endpoint, *adminToken, &results)
	} else {
		runSizes(&cfg, sizes, *cacheFlushBefore, *endpoint, *adminToken, &results)
	}

	printMarkdownSummary(os.Stdout, &results)

	if *outputJSON != "" {
		if err := writeJSON(*outputJSON, &results); err != nil {
			fmt.Fprintf(os.Stderr, "error: write json: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\nResults written to %s\n", *outputJSON)
	}

	// Checked last so the summary and JSON are always produced: a run that
	// blows its budget is exactly the one whose numbers you want to keep.
	if err := enforceErrorBudget(&results, *maxErrorRate, *rampTo > 0); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// enforceErrorBudget reports the first result whose error rate exceeded the
// budget, so a scenario that ran to completion while failing a large share of
// its requests is a failure rather than a successful measurement of one.
//
// Ramp runs are exempt: they drive the system into saturation deliberately,
// and crossing an error threshold is their terminal condition rather than a
// fault.
func enforceErrorBudget(results *sweepResults, maxRate float64, ramp bool) error {
	if ramp || maxRate <= 0 {
		return nil
	}
	for i := range results.Results {
		r := &results.Results[i]
		if r.ErrorRate > maxRate {
			return fmt.Errorf("error budget exceeded: size=%d requested_rps=%d observed %.2f%% errors (budget %.2f%%)",
				r.SizeBytes, r.RequestedRPS, r.ErrorRate*100, maxRate*100)
		}
	}
	return nil
}

// runSizes executes the scenario once per size in sizes, optionally
// flushing the orchestrator cache before each step. Appends each
// result to results.Results.
func runSizes(cfg *scenarioConfig, sizes []int, flushBefore bool, endpoint, adminToken string, results *sweepResults) {
	for _, sz := range sizes {
		if len(sizes) > 1 {
			fmt.Printf("\n=== size=%d bytes ===\n", sz)
		}
		if flushBefore {
			if err := flushAdminCache(endpoint, adminToken); err != nil {
				fmt.Fprintf(os.Stderr, "error: cache flush failed: %v (cold-cache results would be silently warm; check the admin token)\n", err)
				os.Exit(1)
			}
		}
		r, err := runScenario(cfg, sz)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: size=%d: %v\n", sz, err)
			os.Exit(1)
		}
		results.Results = append(results.Results, r)
	}
}

// runRamp drives the same scenario at increasing rates from cfg.rate
// to rampTo (inclusive), stepping by step. Stops early when the
// observed error rate first exceeds errThreshold and records that
// step's rate as the saturation point. Each step optionally
// pre-flushes the cache so saturation reflects the cache-cold
// path rather than steady-state warm hits.
func runRamp(cfg *scenarioConfig, size, rampTo, step int, errThreshold float64, flushBefore bool, endpoint, adminToken string, results *sweepResults) {
	startRate := cfg.rate
	for rate := startRate; rate <= rampTo; rate += step {
		fmt.Printf("\n=== rate=%d req/s ===\n", rate)
		cfg.rate = rate
		if flushBefore {
			if err := flushAdminCache(endpoint, adminToken); err != nil {
				fmt.Fprintf(os.Stderr, "error: cache flush failed: %v (cold-cache results would be silently warm; check the admin token)\n", err)
				os.Exit(1)
			}
		}
		r, err := runScenario(cfg, size)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: rate=%d: %v\n", rate, err)
			os.Exit(1)
		}
		results.Results = append(results.Results, r)
		if r.ErrorRate > errThreshold {
			results.SaturationRPS = rate
			fmt.Printf("\n=== saturation: rate=%d hit error_rate=%.2f%% (> %.2f%% threshold) ===\n",
				rate, r.ErrorRate*100, errThreshold*100)
			return
		}
	}
}

// flushAdminCache POSTs to the orchestrator's cache-flush endpoint so
// each ramp/sweep step starts from a known cold cache state. 503 is
// treated as success since it just means the orchestrator has caching
// disabled, not that the call failed.
func flushAdminCache(endpoint, adminToken string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		endpoint+"/admin/api/cache/flush", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Admin-Token", adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return fmt.Errorf("flush returned %d", resp.StatusCode)
	}
	return nil
}

// parseSizes resolves the effective per-run object sizes. -sizes wins
// when set; otherwise the single -size value produces a one-element
// list so the sweep path covers single-size runs too.
func parseSizes(csv string, single int) ([]int, error) {
	if csv == "" {
		if single <= 0 {
			return nil, fmt.Errorf("size must be > 0, got %d", single)
		}
		return []int{single}, nil
	}
	parts := strings.Split(csv, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid size %q: %w", p, err)
		}
		if n <= 0 {
			return nil, fmt.Errorf("invalid size %d: must be > 0", n)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-sizes parsed to empty list")
	}
	return out, nil
}

// newHardwareInfo captures the host fingerprint at run start so the
// results document remains interpretable when read months later.
func newHardwareInfo() hardwareInfo {
	return hardwareInfo{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		NumCPU:    runtime.NumCPU(),
		GoVersion: runtime.Version(),
	}
}

// runScenario executes one full scenario at a single object size: seeds
// (if needed), attacks, collects vegeta metrics, and returns a runResult.
// The metrics live entirely on the stack so concurrent runs would be
// safe, though main runs them sequentially. cfg is passed by pointer to
// avoid copying the embedded signer/creds on each call.
func runScenario(cfg *scenarioConfig, size int) (runResult, error) {
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		return runResult{}, fmt.Errorf("generate body: %w", err)
	}

	var keys []string
	if needsSeeding(cfg.op) {
		fmt.Printf("Seeding %d objects (%d B each)...\n", cfg.seedCount, size)
		keys = seedObjects(cfg.endpoint, cfg.bucket, cfg.region, cfg.signer, &cfg.creds, body, cfg.seedCount)
		fmt.Printf("Seeded %d objects\n", len(keys))
		if len(keys) == 0 {
			return runResult{}, fmt.Errorf("no objects seeded")
		}
	}

	targeter := newTargeter(cfg, body, keys)
	rate := vegeta.Rate{Freq: cfg.rate, Per: time.Second}
	atk := vegeta.NewAttacker(vegeta.Workers(cfg.workers))

	// Cold mode runs exactly one pass over the working set so every GET is a
	// first touch (the read cache is populated on GET, not on the seeding PUT).
	// The pass length is len(keys)/rate, not the -duration flag.
	attackDuration := cfg.duration
	if cfg.cold && len(keys) > 0 {
		attackDuration = time.Duration(float64(len(keys)) / float64(cfg.rate) * float64(time.Second))
	}

	fmt.Printf("Attacking %s/%s at %d req/s for %s [%s, size=%d]\n",
		cfg.endpoint, cfg.bucket, cfg.rate, attackDuration, cfg.op, size)

	var metrics vegeta.Metrics
	for res := range atk.Attack(targeter, rate, attackDuration, cfg.op) {
		metrics.Add(res)
	}
	metrics.Close()

	fmt.Println()
	_ = vegeta.NewTextReporter(&metrics)(os.Stdout)

	return summarise(size, cfg.rate, &metrics), nil
}

// summarise converts a vegeta.Metrics block into the persisted runResult
// shape. ms-precision percentiles are easier to skim in Markdown than
// vegeta's default mixed time.Duration formatting.
func summarise(size, requestedRate int, m *vegeta.Metrics) runResult {
	statuses := make(map[string]int, len(m.StatusCodes))
	maps.Copy(statuses, m.StatusCodes)
	return runResult{
		SizeBytes:     size,
		RequestedRPS:  requestedRate,
		Requests:      m.Requests,
		DurationSec:   m.Duration.Seconds(),
		ThroughputRPS: m.Rate,
		BytesPerSec:   m.Throughput * float64(size),
		P50Ms:         float64(m.Latencies.P50) / float64(time.Millisecond),
		P95Ms:         float64(m.Latencies.P95) / float64(time.Millisecond),
		P99Ms:         float64(m.Latencies.P99) / float64(time.Millisecond),
		MaxMs:         float64(m.Latencies.Max) / float64(time.Millisecond),
		ErrorRate:     1.0 - m.Success,
		StatusCodes:   statuses,
	}
}

// printMarkdownSummary writes a per-step results table to w. The
// leading column reflects the dimension that varies across rows:
// object size for single/sweep mode, requested rate for ramp mode.
func printMarkdownSummary(w *os.File, r *sweepResults) {
	fmt.Fprintf(w, "\n## %s scenario [%s mode] - duration %s, %d workers\n\n",
		r.Scenario, r.Mode, r.Duration, r.Workers)
	if r.Mode == "ramp" {
		fmt.Fprintln(w, "| Requested RPS | Achieved RPS | Requests | P50 ms | P95 ms | P99 ms | Max ms | Err % |")
		fmt.Fprintln(w, "|---:|---:|---:|---:|---:|---:|---:|---:|")
		for _, x := range r.Results {
			fmt.Fprintf(w, "| %d | %.1f | %d | %.2f | %.2f | %.2f | %.2f | %.2f |\n",
				x.RequestedRPS, x.ThroughputRPS, x.Requests,
				x.P50Ms, x.P95Ms, x.P99Ms, x.MaxMs, x.ErrorRate*100)
		}
		if r.SaturationRPS > 0 {
			fmt.Fprintf(w, "\n**Saturation point:** %d req/s\n", r.SaturationRPS)
		}
		return
	}
	fmt.Fprintln(w, "| Size (B) | Requests | RPS | MB/s | P50 ms | P95 ms | P99 ms | Max ms | Err % |")
	fmt.Fprintln(w, "|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, x := range r.Results {
		fmt.Fprintf(w, "| %d | %d | %.1f | %.2f | %.2f | %.2f | %.2f | %.2f | %.2f |\n",
			x.SizeBytes, x.Requests, x.ThroughputRPS, x.BytesPerSec/(1024*1024),
			x.P50Ms, x.P95Ms, x.P99Ms, x.MaxMs, x.ErrorRate*100)
	}
}

// writeJSON marshals results with indentation so the file is hand-
// readable when committed alongside a perf-envelope writeup.
func writeJSON(path string, r *sweepResults) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// seedObjects uploads n objects and returns the keys that succeeded.
// Retries on 429 with backoff to avoid overwhelming the rate limiter.
func seedObjects(endpoint, bucket, region string, signer *v4.Signer, creds *aws.Credentials, body []byte, n int) []string {
	client := &http.Client{Timeout: 30 * time.Second}
	keys := make([]string, 0, n)

	backoff := time.Duration(0)
	for i := 0; i < n; i++ {
		if backoff > 0 {
			time.Sleep(backoff)
		}

		key := fmt.Sprintf("loadtest/seed-%06d", i)
		reqURL := fmt.Sprintf("%s/%s/%s", endpoint, bucket, key)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, reqURL, bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  seed %d: %v\n", i, err)
			continue
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-Amz-Content-Sha256", unsignedPayload)

		if err := signer.SignHTTP(context.Background(), *creds, req, unsignedPayload, "s3", region, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "  seed %d sign: %v\n", i, err)
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  seed %d: %v\n", i, err)
			continue
		}
		resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			keys = append(keys, key)
			backoff = 0
		case http.StatusTooManyRequests:
			if backoff == 0 {
				backoff = 10 * time.Millisecond
			} else {
				backoff = min(backoff*2, 500*time.Millisecond)
			}
			i-- // retry same index
		default:
			fmt.Fprintf(os.Stderr, "  seed %d: HTTP %d\n", i, resp.StatusCode)
		}
	}
	return keys
}

// newTargeter returns a vegeta.Targeter that generates SigV4-signed S3
// requests. Each call produces a fresh signature so timestamps stay valid.
func newTargeter(cfg *scenarioConfig, body []byte, keys []string) vegeta.Targeter {
	var seq atomic.Uint64

	return func(tgt *vegeta.Target) error {
		n := seq.Add(1)

		switch cfg.op {
		case "put":
			tgt.Method = http.MethodPut
			tgt.URL = fmt.Sprintf("%s/%s/loadtest/%s/obj-%06d", cfg.endpoint, cfg.bucket, cfg.runID, n)
			tgt.Body = body
		case "get":
			tgt.Method = http.MethodGet
			tgt.URL = fmt.Sprintf("%s/%s/%s", cfg.endpoint, cfg.bucket, keys[n%uint64(len(keys))])
			tgt.Body = nil
		case "mixed":
			if n%2 == 0 {
				tgt.Method = http.MethodPut
				tgt.URL = fmt.Sprintf("%s/%s/loadtest/%s/obj-%06d", cfg.endpoint, cfg.bucket, cfg.runID, n)
				tgt.Body = body
			} else {
				tgt.Method = http.MethodGet
				tgt.URL = fmt.Sprintf("%s/%s/%s", cfg.endpoint, cfg.bucket, keys[n%uint64(len(keys))])
				tgt.Body = nil
			}
		case "listobjects":
			tgt.Method = http.MethodGet
			tgt.URL = fmt.Sprintf("%s/%s/?list-type=2&prefix=%s&max-keys=%d",
				cfg.endpoint, cfg.bucket,
				url.QueryEscape(cfg.listPrefix), cfg.listMaxKeys)
			tgt.Body = nil
		case "tagging":
			// Rotates the three subresource verbs over the seeded set so one
			// run exercises the write, the read and the clear rather than
			// measuring whichever happens to be cheapest.
			key := keys[n%uint64(len(keys))]
			tgt.URL = fmt.Sprintf("%s/%s/%s?tagging", cfg.endpoint, cfg.bucket, key)
			switch n % 3 {
			case 0:
				tgt.Method = http.MethodPut
				tgt.Body = []byte(taggingBody)
			case 1:
				tgt.Method = http.MethodGet
				tgt.Body = nil
			default:
				tgt.Method = http.MethodDelete
				tgt.Body = nil
			}
		case "puttagged":
			// A plain PUT plus the inline header, so the run is directly
			// comparable against `put` at the same rate and size: the delta is
			// what tagging on the write path costs.
			tgt.Method = http.MethodPut
			tgt.URL = fmt.Sprintf("%s/%s/loadtest/%s/obj-%06d", cfg.endpoint, cfg.bucket, cfg.runID, n)
			tgt.Body = body
		default:
			return fmt.Errorf("unknown operation: %s", cfg.op)
		}

		// Build a temporary http.Request to sign, then copy headers to the target.
		req, err := http.NewRequestWithContext(context.Background(), tgt.Method, tgt.URL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-Amz-Content-Sha256", unsignedPayload)

		// Set before signing, not after: the orchestrator verifies the
		// signature over the headers the client sent, and x-amz-tagging is one
		// SigV4 covers.
		switch cfg.op {
		case "tagging":
			req.Header.Set("Content-Type", "application/xml")
		case "puttagged":
			req.Header.Set("x-amz-tagging", inlineTaggingHeader)
		}

		if err := cfg.signer.SignHTTP(context.Background(), cfg.creds, req, unsignedPayload, "s3", cfg.region, time.Now()); err != nil {
			return err
		}

		tgt.Header = req.Header
		return nil
	}
}
