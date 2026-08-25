// -------------------------------------------------------------------------------
// Integration Tests - Compressed Object Accounting
//
// Author: Alex Freidah
//
// A compressed object has two sizes: the logical size the client wrote, and the
// smaller stored size the backend actually holds. Only one of them is what the
// backend was charged, and charging the other is the mistake that is invisible
// until a fleet either exhausts a budget early or overruns one.
//
// These tests drive real writes and reads through a compression-enabled proxy
// against real backends, and assert on the counters and the quota the operation
// moved. The ranged read matters most: a range over an encoded object fetches
// only the frames it covers, and a regression to whole-object decode shows up
// in egress and nowhere else.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// compressionEnv is a proxy whose writes are encoded, plus the manager behind
// it so a test can read the counters the requests moved.
type compressionEnv struct {
	client *s3.Client
	mgr    *proxy.BackendManager
}

// compTestChunk is the chunk size these tests encode at. The smallest the
// codec allows, so a modest fixture still spans several frames and a ranged
// read has more than one frame to choose between.
const compTestChunk = compression.MinChunkSize

// setupCompressionEnv builds a compression-enabled manager over the shared
// backends and store, fronted by its own proxy server.
func setupCompressionEnv(t *testing.T) *compressionEnv {
	t.Helper()
	resetState(t)
	setQuotaLimits(t, 64<<20)
	ctx := context.Background()

	codec, err := compression.NewCodec(compression.DefaultLevel, compTestChunk)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(codec.Close)

	stores := newStores(testStore)
	mgr := proxytest.NewManager(t, stores, &proxy.BackendManagerConfig{
		Storage: proxy.StorageDeps{
			Backends: testBackends,
			Order:    testBackendOrder,
		},
		Policies: proxy.PolicyConfig{
			CacheTTL:        60 * time.Second,
			BackendTimeout:  30 * time.Second,
			RoutingStrategy: config.RoutingPack,
		},
		Features: proxy.FeatureDeps{
			Codec: codec,
			Compression: config.CompressionConfig{
				Enabled:   true,
				Level:     "default",
				ChunkSize: compTestChunk,
				MinSize:   1,
				MinRatio:  config.DefaultCompressionMinRatio,
			},
		},
		Operations: proxy.OperationalDeps{
			Metrics: newMetricsAdapter(testStore),
		},
	})

	srv := &s3api.Server{Objects: mgr.Objects(), Multipart: mgr.Multipart()}
	srv.SetBucketAuth(mustBucketRegistry(t, []config.BucketConfig{
		{
			Name:        virtualBucket,
			Credentials: []config.CredentialConfig{{AccessKeyID: "test", SecretAccessKey: "test"}},
		},
	}))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: srv}
	go server.Serve(listener)
	t.Cleanup(func() { server.Shutdown(ctx) })

	return &compressionEnv{
		client: s3.New(s3.Options{
			BaseEndpoint: aws.String("http://" + listener.Addr().String()),
			Region:       "us-east-1",
			Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
			UsePathStyle: true,
		}),
		mgr: mgr,
	}
}

// compressible returns n bytes the codec can shrink substantially, so stored
// and logical sizes are far enough apart that confusing them is unmistakable.
func compressible(n int) []byte {
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	out := make([]byte, 0, n+len(line))
	for len(out) < n {
		out = append(out, line...)
	}
	return out[:n]
}

// partlyCompressible returns n bytes that shrink by roughly half rather than
// by orders of magnitude.
//
// Repetitive input encodes to so little that a single frame plus the seek table
// accounts for nearly the whole stored object, which leaves a ranged read
// indistinguishable from a whole-object fetch. Any pattern the encoder can
// model does the same, so half of every block is drawn from a seeded PRNG that
// it cannot, and half is constant so the object still clears the ratio floor
// and is stored encoded at all. Seeded, so the sizes are the same every run.
func partlyCompressible(n int) []byte {
	rng := rand.New(rand.NewSource(1))
	out := make([]byte, n)
	const block = 4096
	for start := 0; start < n; start += block {
		end := min(start+block, n)
		mid := min(start+block/2, end)
		for i := start; i < mid; i++ {
			out[i] = byte(rng.Intn(256))
		}
		for i := mid; i < end; i++ {
			out[i] = 'z'
		}
	}
	return out
}

// incompressible returns n bytes the encoder cannot shrink, so the object is
// stored verbatim at exactly n and any quota arithmetic over it is exact rather
// than dependent on what the encoder happened to achieve. Seeded, so a failure
// reproduces.
func incompressible(n int) []byte {
	rng := rand.New(rand.NewSource(2))
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(rng.Intn(256))
	}
	return out
}

// putCompressed writes body through the compression-enabled proxy and returns
// the key, the backend it landed on, and its stored size.
func (e *compressionEnv) putCompressed(t *testing.T, prefix string, body []byte) (key, backendName string, stored int64) {
	t.Helper()
	key = uniqueKey(t, prefix)
	if _, err := e.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	backendName = queryObjectBackend(t, key)
	return key, backendName, queryStoredSize(t, key)
}

// TestCompressionUsage_PutChargesStoredSizeNotLogical is the headline. The
// client wrote the logical size, but only the encoded bytes reached the
// backend, and that is what its bandwidth budget was spent on. Charging the
// logical size would have a fleet refusing writes long before the provider
// would.
func TestCompressionUsage_PutChargesStoredSizeNotLogical(t *testing.T) {
	env := setupCompressionEnv(t)
	body := compressible(compTestChunk * 4)

	var key, target string
	var stored int64
	deltas := fleetUsageDelta(env.mgr, testBackendOrder, func() {
		key, target, stored = env.putCompressed(t, "comp-usage-put", body)
	})

	if stored >= int64(len(body)) {
		t.Fatalf("stored size %d did not shrink below logical %d; the fixture is not compressible",
			stored, len(body))
	}
	assertCharged(t, "compressed PUT on "+target, deltas[target],
		usageSnapshot{APICalls: 1, Ingress: stored})
	if got := queryLogicalSize(t, key); got != int64(len(body)) {
		t.Errorf("logical_size = %d, want %d", got, len(body))
	}
}

// TestCompressionUsage_PutChargesStoredSizeToQuota asserts the storage ledger
// agrees with the bandwidth one: a compressed object occupies its stored size,
// so that is what comes off the backend's capacity.
func TestCompressionUsage_PutChargesStoredSizeToQuota(t *testing.T) {
	env := setupCompressionEnv(t)
	body := compressible(compTestChunk * 4)

	before := map[string]int64{}
	for _, name := range testBackendOrder {
		before[name] = queryQuotaUsed(t, name)
	}

	_, target, stored := env.putCompressed(t, "comp-usage-quota", body)

	if got := queryQuotaUsed(t, target) - before[target]; got != stored {
		t.Errorf("bytes_used moved by %d, want the stored size %d", got, stored)
	}
}

// TestCompressionUsage_GetChargesStoredSize asserts a full read of an encoded
// object charges the bytes that left the backend, not the larger object the
// client receives after decoding.
func TestCompressionUsage_GetChargesStoredSize(t *testing.T) {
	env := setupCompressionEnv(t)
	body := compressible(compTestChunk * 4)
	key, target, stored := env.putCompressed(t, "comp-usage-get", body)

	var got []byte
	delta := usageDelta(env.mgr, target, func() {
		out, err := env.client.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		defer out.Body.Close()
		got, err = io.ReadAll(out.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
	})

	if !bytes.Equal(got, body) {
		t.Fatalf("read back %d bytes, want the %d written", len(got), len(body))
	}
	if delta.Egress != stored {
		t.Errorf("egress = %d, want the stored size %d (the client's %d bytes never crossed the backend link)",
			delta.Egress, stored, len(body))
	}
}

// TestCompressionUsage_RangeFetchesOnlyCoveredFrames is what the seek table
// buys. A short range near the start must not pull the whole object off the
// backend; the egress it spends is bounded by the frames the range covers.
// A regression to whole-object decode is invisible everywhere except here and
// the bandwidth bill.
func TestCompressionUsage_RangeFetchesOnlyCoveredFrames(t *testing.T) {
	env := setupCompressionEnv(t)
	body := partlyCompressible(compTestChunk * 16)
	key, target, stored := env.putCompressed(t, "comp-usage-range", body)

	const want = 64
	var got []byte
	delta := usageDelta(env.mgr, target, func() {
		out, err := env.client.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
			Range:  aws.String("bytes=0-63"),
		})
		if err != nil {
			t.Fatalf("ranged GetObject: %v", err)
		}
		defer out.Body.Close()
		got, err = io.ReadAll(out.Body)
		if err != nil {
			t.Fatalf("read ranged body: %v", err)
		}
	})

	if !bytes.Equal(got, body[:want]) {
		t.Fatalf("ranged read returned %d bytes that do not match the first %d written", len(got), want)
	}
	// Half the stored object is a deliberately loose bound. The exact figure is
	// the covering frame plus the seek table, which moves with chunk size and
	// encoder output; what must not happen is the whole object coming across to
	// serve 64 bytes.
	if delta.Egress >= stored/2 {
		t.Errorf("ranged read spent %d egress against a stored size of %d; %d bytes should cost roughly one frame",
			delta.Egress, stored, want)
	}
}
