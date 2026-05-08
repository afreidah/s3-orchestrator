// -------------------------------------------------------------------------------
// Chunk-Framing Live-Cluster Scan
//
// Author: Alex Freidah
//
// Build-tagged diagnostic that walks every visible bucket on a live S3
// orchestrator and probes each object's head for AWS chunked-encoding
// framing. Reports each finding via t.Errorf so a single run surfaces all
// corrupt objects. Reads endpoint and credentials from environment
// variables; skips when DIAG_S3_ENDPOINT is unset.
//
// Invocation:
//
//	DIAG_S3_ENDPOINT=https://s3.example.com \
//	DIAG_S3_ACCESS_KEY=... DIAG_S3_SECRET_KEY=... \
//	go test -tags=diag -run TestScanChunkedFraming -v \
//	    ./internal/integration/chunkframing/...
// -------------------------------------------------------------------------------

//go:build diag

package chunkframing

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// scanConfig captures the operator-supplied parameters for a diagnostic
// run.
type scanConfig struct {
	Endpoint   string
	AccessKey  string
	SecretKey  string
	Region     string
	Bucket     string
	Prefix     string
	MaxObjects int64
	ProbeBytes int
}

// -------------------------------------------------------------------------
// ENTRY
// -------------------------------------------------------------------------

// TestScanChunkedFraming walks every visible bucket and reports every
// object whose head looks like aws-chunked framing as a t.Errorf, so a
// single test run lists all corrupt objects.
func TestScanChunkedFraming(t *testing.T) {
	cfg, ok := loadConfigFromEnv()
	if !ok {
		t.Skip("DIAG_S3_ENDPOINT, DIAG_S3_ACCESS_KEY, DIAG_S3_SECRET_KEY required")
	}
	ctx := context.Background()
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(cfg.Endpoint),
		Region:       cfg.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		UsePathStyle: true,
	})

	buckets, err := resolveBuckets(ctx, client, cfg.Bucket)
	if err != nil {
		t.Fatalf("list buckets: %v", err)
	}
	t.Logf("scanning buckets=%d max=%d probe=%d", len(buckets), cfg.MaxObjects, cfg.ProbeBytes)

	var scanned, hits int64
	for _, bucket := range buckets {
		if scanned >= cfg.MaxObjects {
			break
		}
		s, h, err := scanBucket(ctx, t, client, bucket, cfg.Prefix, cfg.ProbeBytes,
			cfg.MaxObjects-scanned)
		scanned += s
		hits += h
		if err != nil {
			t.Errorf("scan %s: %v", bucket, err)
		}
	}
	t.Logf("scanned=%d corrupt=%d", scanned, hits)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// loadConfigFromEnv reads scan parameters from environment variables.
// Returns ok=false when the required minimum (endpoint + credentials) is
// unset.
func loadConfigFromEnv() (scanConfig, bool) {
	cfg := scanConfig{
		Endpoint:   os.Getenv("DIAG_S3_ENDPOINT"),
		AccessKey:  os.Getenv("DIAG_S3_ACCESS_KEY"),
		SecretKey:  os.Getenv("DIAG_S3_SECRET_KEY"),
		Region:     os.Getenv("DIAG_S3_REGION"),
		Bucket:     os.Getenv("DIAG_S3_BUCKET"),
		Prefix:     os.Getenv("DIAG_S3_PREFIX"),
		MaxObjects: 1_000_000,
		ProbeBytes: MinProbeBytes,
	}
	if cfg.Endpoint == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return cfg, false
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if v := os.Getenv("DIAG_MAX"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.MaxObjects = n
		}
	}
	if v := os.Getenv("DIAG_PROBE_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 4096 {
			cfg.ProbeBytes = n
		}
	}
	return cfg, true
}

// resolveBuckets returns the explicit bucket name when given, otherwise
// the full set returned by ListBuckets.
func resolveBuckets(ctx context.Context, client *s3.Client, only string) ([]string, error) {
	if only != "" {
		return []string{only}, nil
	}
	resp, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Buckets))
	for _, b := range resp.Buckets {
		if b.Name != nil {
			out = append(out, *b.Name)
		}
	}
	return out, nil
}

// scanBucket lists keys under the given bucket+prefix and inspects each
// object's head, reporting every framed object via t.Errorf. Returns the
// counts of scanned and corrupt objects.
func scanBucket(
	ctx context.Context,
	t *testing.T,
	client *s3.Client,
	bucket, prefix string,
	probeBytes int,
	limit int64,
) (scanned, hits int64, err error) {
	t.Helper()
	if limit <= 0 {
		return 0, 0, nil
	}
	pager := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() && scanned < limit {
		page, perr := pager.NextPage(ctx)
		if perr != nil {
			return scanned, hits, fmt.Errorf("list page: %w", perr)
		}
		for _, obj := range page.Contents {
			if scanned >= limit {
				return scanned, hits, nil
			}
			scanned++
			if probeAndReport(ctx, t, client, bucket, &obj, probeBytes) {
				hits++
			}
		}
	}
	return scanned, hits, nil
}

// probeAndReport fetches and inspects one object's head bytes. Returns
// true when chunk framing was detected and reported via t.Errorf;
// returns false when the object is empty/keyless, the head fetch
// failed, or the bytes look like ordinary content.
func probeAndReport(
	ctx context.Context,
	t *testing.T,
	client *s3.Client,
	bucket string,
	obj *s3types.Object,
	probeBytes int,
) bool {
	t.Helper()
	if obj.Key == nil || (obj.Size != nil && *obj.Size == 0) {
		return false
	}
	head, err := fetchHead(ctx, client, bucket, *obj.Key, probeBytes)
	if err != nil {
		t.Logf("warn: head %s/%s: %v", bucket, *obj.Key, err)
		return false
	}
	variant := Detect(head)
	if variant == VariantNone {
		return false
	}
	snippet := head
	if len(snippet) > 64 {
		snippet = snippet[:64]
	}
	size := int64(0)
	if obj.Size != nil {
		size = *obj.Size
	}
	t.Errorf("corrupt: bucket=%s key=%s size=%d variant=%s head=%s",
		bucket, *obj.Key, size, variant, hex.EncodeToString(snippet))
	return true
}

// fetchHead retrieves the first n bytes of an object via Range GET.
func fetchHead(ctx context.Context, client *s3.Client, bucket, key string, n int) ([]byte, error) {
	rng := fmt.Sprintf("bytes=0-%d", n-1)
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Range:  aws.String(rng),
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, int64(n)))
}
