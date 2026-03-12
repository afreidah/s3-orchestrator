// -------------------------------------------------------------------------------
// S3 Orchestrator Load Tester
//
// Author: Alex Freidah
//
// Vegeta-based load testing tool with SigV4 authentication for the S3 API.
// Supports constant-rate PUT, GET, and mixed workloads against any
// S3-compatible endpoint.
//
// Usage:
//
//	go run . -op put -rate 200 -duration 30s -size 4096
//	go run . -op get -rate 500 -duration 1m -seed 1000
//	go run . -op mixed -rate 300 -duration 2m -seed 500
// -------------------------------------------------------------------------------
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	vegeta "github.com/tsenart/vegeta/v12/lib"
)

// unsignedPayload tells the server to skip payload hash verification.
// Used for all methods — this is a load tester, not a security tool.
const unsignedPayload = "UNSIGNED-PAYLOAD"

func main() {
	var (
		endpoint  = flag.String("endpoint", "http://localhost:9000", "S3 orchestrator endpoint")
		accessKey = flag.String("access-key", "photoskey", "Access key ID")
		secretKey = flag.String("secret-key", "photossecret", "Secret access key")
		bucket    = flag.String("bucket", "photos", "Target bucket")
		region    = flag.String("region", "us-east-1", "AWS region for SigV4")
		rateFlag  = flag.Int("rate", 100, "Requests per second")
		dur       = flag.Duration("duration", 30*time.Second, "Test duration")
		size      = flag.Int("size", 1024, "Object size in bytes for PUT")
		op        = flag.String("op", "put", "Operation: put, get, mixed")
		workers   = flag.Uint64("workers", 10, "Concurrent workers")
		seedN     = flag.Int("seed", 100, "Objects to pre-seed for get/mixed")
	)
	flag.Parse()

	signer := v4.NewSigner()
	creds := aws.Credentials{
		AccessKeyID:     *accessKey,
		SecretAccessKey: *secretKey,
	}

	// Pre-generate a random body reused across all PUT requests.
	body := make([]byte, *size)
	if _, err := rand.Read(body); err != nil {
		fmt.Fprintf(os.Stderr, "error: generate body: %v\n", err)
		os.Exit(1)
	}

	// Seed objects for GET / mixed workloads.
	var keys []string
	if *op == "get" || *op == "mixed" {
		fmt.Printf("Seeding %d objects (%d B each)...\n", *seedN, *size)
		keys = seedObjects(*endpoint, *bucket, *region, signer, creds, body, *seedN)
		fmt.Printf("Seeded %d objects\n", len(keys))
		if len(keys) == 0 {
			fmt.Fprintln(os.Stderr, "error: no objects seeded")
			os.Exit(1)
		}
	}

	targeter := newTargeter(*endpoint, *bucket, *region, *op, signer, creds, body, keys)
	rate := vegeta.Rate{Freq: *rateFlag, Per: time.Second}
	atk := vegeta.NewAttacker(vegeta.Workers(*workers))

	fmt.Printf("Attacking %s/%s at %d req/s for %s [%s]\n",
		*endpoint, *bucket, *rateFlag, *dur, *op)

	var metrics vegeta.Metrics
	for res := range atk.Attack(targeter, rate, *dur, *op) {
		metrics.Add(res)
	}
	metrics.Close()

	fmt.Println()
	_ = vegeta.NewTextReporter(&metrics)(os.Stdout)
}

// seedObjects uploads n objects and returns the keys that succeeded.
// Retries on 429 with backoff to avoid overwhelming the rate limiter.
func seedObjects(endpoint, bucket, region string, signer *v4.Signer, creds aws.Credentials, body []byte, n int) []string {
	client := &http.Client{Timeout: 30 * time.Second}
	keys := make([]string, 0, n)

	backoff := time.Duration(0)
	for i := 0; i < n; i++ {
		if backoff > 0 {
			time.Sleep(backoff)
		}

		key := fmt.Sprintf("loadtest/seed-%06d", i)
		reqURL := fmt.Sprintf("%s/%s/%s", endpoint, bucket, key)

		req, err := http.NewRequest(http.MethodPut, reqURL, bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  seed %d: %v\n", i, err)
			continue
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-Amz-Content-Sha256", unsignedPayload)

		if err := signer.SignHTTP(context.Background(), creds, req, unsignedPayload, "s3", region, time.Now()); err != nil {
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
func newTargeter(endpoint, bucket, region, op string, signer *v4.Signer, creds aws.Credentials, body []byte, keys []string) vegeta.Targeter {
	var seq atomic.Uint64

	return func(tgt *vegeta.Target) error {
		n := seq.Add(1)

		switch op {
		case "put":
			tgt.Method = http.MethodPut
			tgt.URL = fmt.Sprintf("%s/%s/loadtest/obj-%06d", endpoint, bucket, n)
			tgt.Body = body
		case "get":
			tgt.Method = http.MethodGet
			tgt.URL = fmt.Sprintf("%s/%s/%s", endpoint, bucket, keys[n%uint64(len(keys))])
			tgt.Body = nil
		case "mixed":
			if n%2 == 0 {
				tgt.Method = http.MethodPut
				tgt.URL = fmt.Sprintf("%s/%s/loadtest/obj-%06d", endpoint, bucket, n)
				tgt.Body = body
			} else {
				tgt.Method = http.MethodGet
				tgt.URL = fmt.Sprintf("%s/%s/%s", endpoint, bucket, keys[n%uint64(len(keys))])
				tgt.Body = nil
			}
		default:
			return fmt.Errorf("unknown operation: %s", op)
		}

		// Build a temporary http.Request to sign, then copy headers to the target.
		req, err := http.NewRequest(tgt.Method, tgt.URL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-Amz-Content-Sha256", unsignedPayload)

		if err := signer.SignHTTP(context.Background(), creds, req, unsignedPayload, "s3", region, time.Now()); err != nil {
			return err
		}

		tgt.Header = req.Header
		return nil
	}
}
