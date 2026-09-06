// -------------------------------------------------------------------------------
// Integration Test Harness - Isolated Per-Test Orchestrators
//
// Author: Alex Freidah
//
// The shared fixture in helpers_test.go hands every test one database, one
// backend fleet and one set of quota numbers. A test needing different ones has
// to edit the shared rows and put them back, which holds only for as long as no
// two tests disagree about what the fleet should look like.
//
// A harness is an orchestrator of its own: its own Postgres database, its own
// backend buckets, its own manager, workers and proxy. It declares the fleet it
// wants instead of editing the one already there, so nothing it does is visible
// to another test and nothing another test does reaches it - no resetState, no
// quota save-and-restore, no shared keyspace.
//
// The containers stay shared. A database and a bucket cost milliseconds where a
// container costs seconds, and isolation at the container level would only buy
// something for a property of the container itself - TLS termination, a
// different S3 implementation, an endpoint that is genuinely unreachable. No
// test built on this harness is testing one of those.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/postgres"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// harnessSeq names each harness's database and buckets apart from every other
// one in the run.
var harnessSeq atomic.Int64

// harnessBackend is one backend in a declared fleet. The endpoint, bucket and
// credentials belong to the harness; a test only says how much room it has.
type harnessBackend struct {
	Name  string
	Quota int64
}

// harnessSpec declares the world one test needs. The zero value is a two-backend
// plaintext fleet with no stored-form layers applied.
type harnessSpec struct {
	Backends       []harnessBackend
	Compression    *config.CompressionConfig
	Encrypt        bool
	Integrity      *config.IntegrityConfig
	Routing        config.RoutingStrategy
	Pending        bool
	CopiesPerWrite int // above 1, a PUT places its own copies instead of leaving them to the replicator
}

// harness is one isolated orchestrator and everything a test needs to reach
// into it: the client that drives it, the stack and workers behind it, and a
// direct handle on its own database.
type harness struct {
	t        *testing.T
	client   *s3.Client
	stack    *proxytest.Stack
	workers  *proxytest.Workers
	store    *postgres.Store
	db       *sql.DB
	enc      *encryption.Encryptor
	codec    *compression.Codec
	backends map[string]s3be.ObjectBackend
	order    []string
}

// defaultHarnessBackends is the fleet a spec that names none gets: two
// backends with room enough that placement never becomes the subject of a test
// that did not ask for it.
func defaultHarnessBackends() []harnessBackend {
	return []harnessBackend{
		{Name: "h-minio-1", Quota: 64 << 20},
		{Name: "h-minio-2", Quota: 64 << 20},
	}
}

// newHarness builds an isolated orchestrator to spec and registers its teardown.
func newHarness(t *testing.T, spec harnessSpec) *harness {
	t.Helper()
	ctx := context.Background()

	id := harnessSeq.Add(1)
	dbName := fmt.Sprintf("harness_%d", id)
	if _, err := testDB.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatalf("create harness database %s: %v", dbName, err)
	}

	dbCfg := testDBConfig
	dbCfg.Database = dbName
	st, err := postgres.NewStore(ctx, &dbCfg, store.NewDatabaseBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
		OpenTimeout:      500 * time.Millisecond,
		CacheTTL:         60 * time.Second,
	}))
	if err != nil {
		t.Fatalf("open harness store: %v", err)
	}
	if err := st.RunMigrations(ctx); err != nil {
		t.Fatalf("migrate harness database: %v", err)
	}

	backendCfgs := harnessBackendConfigs(t, ctx, id, spec)
	if err := st.SyncQuotaLimits(ctx, backendCfgs); err != nil {
		t.Fatalf("sync harness quota limits: %v", err)
	}

	h := &harness{t: t, store: st, backends: map[string]s3be.ObjectBackend{}}
	for i := range backendCfgs {
		be, err := s3be.NewS3Backend(ctx, &backendCfgs[i])
		if err != nil {
			t.Fatalf("build harness backend %s: %v", backendCfgs[i].Name, err)
		}
		h.backends[backendCfgs[i].Name] = be
		h.order = append(h.order, backendCfgs[i].Name)
	}

	h.buildStack(t, spec)

	h.db, err = sql.Open("pgx", dbCfg.ConnectionString())
	if err != nil {
		t.Fatalf("open harness sql handle: %v", err)
	}

	h.client = s3.New(s3.Options{
		BaseEndpoint: aws.String("http://" + h.serve(t, ctx)),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
	})

	t.Cleanup(func() {
		h.db.Close()
		st.Close()
		// FORCE because a pgx pool connection can outlive Close by a moment,
		// and a harness that cannot drop its database would leak it into every
		// later run against the same container.
		if _, err := testDB.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)"); err != nil {
			t.Errorf("drop harness database %s: %v", dbName, err)
		}
	})
	return h
}

// harnessBackendConfigs turns the declared fleet into backend configs, creating
// a fresh bucket for each so no two harnesses share a keyspace. Backends are
// spread across the available MinIO containers so a fleet larger than one still
// exercises distinct endpoints.
func harnessBackendConfigs(t *testing.T, ctx context.Context, id int64, spec harnessSpec) []config.BackendConfig {
	t.Helper()
	declared := spec.Backends
	if len(declared) == 0 {
		declared = defaultHarnessBackends()
	}
	if len(declared) > len(minioEndpoints) {
		t.Fatalf("harness declares %d backends but only %d MinIO containers are running",
			len(declared), len(minioEndpoints))
	}

	out := make([]config.BackendConfig, len(declared))
	for i, be := range declared {
		endpoint := minioEndpoints[i%len(minioEndpoints)]
		bucket := fmt.Sprintf("harness-%d-%d", id, i)
		mustCreateHarnessBucket(t, ctx, endpoint, bucket)
		out[i] = config.BackendConfig{
			Name:            be.Name,
			Endpoint:        endpoint,
			Region:          "us-east-1",
			Bucket:          bucket,
			AccessKeyID:     "minioadmin",
			SecretAccessKey: "minioadmin",
			ForcePathStyle:  true,
			QuotaBytes:      be.Quota,
		}
	}
	return out
}

// mustCreateHarnessBucket creates one harness-owned bucket on a MinIO container.
func mustCreateHarnessBucket(t *testing.T, ctx context.Context, endpoint, bucket string) {
	t.Helper()
	mc := s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", ""),
		UsePathStyle: true,
	})
	if _, err := mc.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create harness bucket %s: %v", bucket, err)
	}
}

// buildStack assembles the proxy stack and workers for a spec, wiring
// whichever stored-form layers it asked for.
func (h *harness) buildStack(t *testing.T, spec harnessSpec) {
	t.Helper()

	opts := proxytest.StackOptions{
		CacheTTL:       60 * time.Second,
		BackendTimeout: 30 * time.Second,
		CopiesPerWrite: spec.CopiesPerWrite,
	}
	if spec.Encrypt {
		provider, err := encryption.NewConfigKeyProvider(testMasterKey, "test-key")
		if err != nil {
			t.Fatalf("harness key provider: %v", err)
		}
		h.enc, err = encryption.NewEncryptor(provider, 65536)
		if err != nil {
			t.Fatalf("harness encryptor: %v", err)
		}
		opts.Encryptor = h.enc
	}
	if spec.Compression != nil {
		codec, err := compression.NewCodec(compression.DefaultLevel, spec.Compression.ChunkSize)
		if err != nil {
			t.Fatalf("harness codec: %v", err)
		}
		t.Cleanup(codec.Close)
		h.codec = codec
		opts.Codec = codec
		opts.Compression = *spec.Compression
	}

	routing := spec.Routing
	if routing == "" {
		routing = config.RoutingPack
	}

	stores := newStores(h.store)
	opts.Runtime = proxytest.NewRuntime(&proxytest.RuntimeOptions{
		Backends:        h.backends,
		Order:           h.order,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: routing,
		Metrics:         newMetricsAdapter(h.store),
	})
	h.stack = proxytest.New(t, stores, &opts)
	// The tracker starts empty, so admission would judge every write against a
	// zero ceiling. Production primes it from backend_quotas before serving;
	// the harness has to do the same or the limits SyncQuotaLimits just wrote
	// are invisible to routing.
	if err := h.stack.Usage.RefreshQuotaBaselines(context.Background()); err != nil {
		t.Fatalf("prime harness quota baselines: %v", err)
	}
	if spec.Integrity != nil {
		h.stack.IntegrityCfg.Store(spec.Integrity)
	}
	// The workers get the same stored-form layers the stack writes with, so
	// the scrubber can undo them; without that it cannot read the objects this
	// harness stores and reports a clean sweep having checked none of them.
	h.workers = proxytest.BuildWorkersWithFeatures(h.stack, stores, proxytest.WorkerFeatures{
		Encryptor: h.enc,
		Codec:     h.codec,
	})
}

// serve starts the harness's own S3 endpoint and returns its address.
func (h *harness) serve(t *testing.T, ctx context.Context) string {
	t.Helper()
	srv := &s3api.Server{Objects: h.stack.Objects, Multipart: h.stack.Multipart}
	srv.SetBucketAuth(mustBucketRegistry(t, []config.BucketConfig{{
		Name:        virtualBucket,
		Credentials: []config.CredentialConfig{{AccessKeyID: "test", SecretAccessKey: "test"}},
	}}))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("harness listen: %v", err)
	}
	server := &http.Server{Handler: srv, ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute}
	go server.Serve(listener)
	t.Cleanup(func() { server.Shutdown(ctx) })
	return listener.Addr().String()
}

// -------------------------------------------------------------------------
// DRIVING A HARNESS
// -------------------------------------------------------------------------

// put writes body under key and returns the backend that took it.
func (h *harness) put(key string, body []byte) string {
	h.t.Helper()
	if _, err := h.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		h.t.Fatalf("PutObject(%s, %d bytes): %v", key, len(body), err)
	}
	return h.objectBackend(key)
}

// get reads key back in full.
func (h *harness) get(key string) []byte {
	h.t.Helper()
	return h.getRange(key, "")
}

// getRange reads key, optionally through a Range header. An empty rangeHeader
// is a whole-object read.
func (h *harness) getRange(key, rangeHeader string) []byte {
	h.t.Helper()
	in := &s3.GetObjectInput{Bucket: aws.String(virtualBucket), Key: aws.String(key)}
	if rangeHeader != "" {
		in.Range = aws.String(rangeHeader)
	}
	out, err := h.client.GetObject(context.Background(), in)
	if err != nil {
		h.t.Fatalf("GetObject(%s, range=%q): %v", key, rangeHeader, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		h.t.Fatalf("read body of %s: %v", key, err)
	}
	return body
}

// -------------------------------------------------------------------------
// READING A HARNESS LEDGER
// -------------------------------------------------------------------------

// objectBackend returns which backend holds key.
func (h *harness) objectBackend(key string) string {
	h.t.Helper()
	var name string
	if err := h.db.QueryRow(
		"SELECT backend_name FROM object_locations WHERE object_key = $1", internalKey(key),
	).Scan(&name); err != nil {
		h.t.Fatalf("objectBackend(%s): %v", key, err)
	}
	return name
}

// storedSize returns what the ledger says a backend holds for key.
func (h *harness) storedSize(key string) int64 {
	h.t.Helper()
	var size int64
	if err := h.db.QueryRow(
		"SELECT size_bytes FROM object_locations WHERE object_key = $1", internalKey(key),
	).Scan(&size); err != nil {
		h.t.Fatalf("storedSize(%s): %v", key, err)
	}
	return size
}

// compressionAlgorithm returns the encoding key is stored in, empty for a copy
// held verbatim.
func (h *harness) compressionAlgorithm(key string) string {
	h.t.Helper()
	var algorithm sql.NullString
	if err := h.db.QueryRow(
		"SELECT compression_algorithm FROM object_locations WHERE object_key = $1", internalKey(key),
	).Scan(&algorithm); err != nil {
		h.t.Fatalf("compressionAlgorithm(%s): %v", key, err)
	}
	return algorithm.String
}

// flushQuota drains the in-memory byte counter into backend_quotas and
// re-primes the baselines from the rows it wrote. bytes_used is eventually
// consistent, so anything reading that column has to ask for the flush the
// usage service would otherwise run on its own tick.
func (h *harness) flushQuota() {
	h.t.Helper()
	if err := h.stack.Usage.FlushQuota(context.Background()); err != nil {
		h.t.Fatalf("flushQuota: %v", err)
	}
}

// quotaUsed returns a backend's committed byte count.
func (h *harness) quotaUsed(backendName string) int64 {
	h.t.Helper()
	h.flushQuota()
	var used int64
	if err := h.db.QueryRow(
		`SELECT GREATEST(0, COALESCE(SUM(bytes_used), 0)) FROM backend_quota_stripes WHERE backend_name = $1`,
		backendName,
	).Scan(&used); err != nil {
		h.t.Fatalf("quotaUsed(%s): %v", backendName, err)
	}
	return used
}

// backendSize reports how many bytes a backend physically holds for key, read
// off the backend rather than out of the ledger.
func (h *harness) backendSize(backendName, key string) int64 {
	h.t.Helper()
	be, ok := h.backends[backendName]
	if !ok {
		h.t.Fatalf("backendSize: %q is not in this harness fleet", backendName)
	}
	head, err := be.HeadObject(context.Background(), internalKey(key))
	if err != nil {
		h.t.Fatalf("backendSize(%s, %s): %v", backendName, key, err)
	}
	return head.Size
}

// locationsOn counts the ledger rows pointing at a backend.
func (h *harness) locationsOn(backendName string) int {
	h.t.Helper()
	var count int
	if err := h.db.QueryRow(
		"SELECT COUNT(*) FROM object_locations WHERE backend_name = $1", backendName,
	).Scan(&count); err != nil {
		h.t.Fatalf("locationsOn(%s): %v", backendName, err)
	}
	return count
}

// objectBackends returns every backend holding a copy of key, oldest first.
func (h *harness) objectBackends(key string) []string {
	h.t.Helper()
	rows, err := h.db.Query(
		"SELECT backend_name FROM object_locations WHERE object_key = $1 ORDER BY created_at ASC",
		internalKey(key))
	if err != nil {
		h.t.Fatalf("objectBackends(%s): %v", key, err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			h.t.Fatalf("objectBackends scan: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("objectBackends(%s): %v", key, err)
	}
	return names
}

// hashedCopies counts the copies of key carrying a stored content hash, which
// is what a later scrub compares the backend bytes against.
func (h *harness) hashedCopies(key string) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(
		`SELECT count(*) FROM object_locations
		 WHERE object_key = $1 AND content_hash IS NOT NULL AND content_hash <> ''`,
		internalKey(key),
	).Scan(&n); err != nil {
		h.t.Fatalf("hashedCopies(%s): %v", key, err)
	}
	return n
}

// setQuota changes a backend's capacity mid-test. Safe to do bluntly here in a
// way it is not against the shared fixture: this row belongs to one harness, so
// nothing else is reading it.
func (h *harness) setQuota(backendName string, limit int64) {
	h.t.Helper()
	if _, err := h.db.Exec(
		"UPDATE backend_quotas SET bytes_limit = $1, updated_at = NOW() WHERE backend_name = $2",
		limit, backendName,
	); err != nil {
		h.t.Fatalf("setQuota(%s, %d): %v", backendName, limit, err)
	}
	// Admission reads the ceiling from the tracker, not the row, so the new
	// limit means nothing until the baseline is reloaded.
	if err := h.stack.Usage.RefreshQuotaBaselines(context.Background()); err != nil {
		h.t.Fatalf("setQuota(%s, %d): refresh baselines: %v", backendName, limit, err)
	}
}

// -------------------------------------------------------------------------
// DRIVING WORKERS
// -------------------------------------------------------------------------

// replicate runs one replication pass to the given factor.
func (h *harness) replicate(factor int) {
	h.t.Helper()
	if _, err := h.workers.Replicator.Replicate(context.Background(), config.ReplicationConfig{
		Factor:         factor,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}, nil); err != nil {
		h.t.Fatalf("Replicate(factor=%d): %v", factor, err)
	}
}

// waitDrainComplete polls until the drain of backendName reports inactive.
func (h *harness) waitDrainComplete(backendName string, timeout time.Duration) {
	h.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		progress, err := h.stack.Drain.GetDrainProgress(ctx, backendName)
		if err != nil {
			h.t.Fatalf("GetDrainProgress(%s): %v", backendName, err)
		}
		if !progress.Active {
			if progress.Error != "" {
				h.t.Fatalf("drain of %s failed: %s", backendName, progress.Error)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("drain of %s did not complete within %s", backendName, timeout)
}

// -------------------------------------------------------------------------
// BYTE VERIFICATION
// -------------------------------------------------------------------------

// corrupt overwrites a copy's bytes on one backend behind the orchestrator's
// back, standing in for bit rot or a bad write the ledger knows nothing about.
func (h *harness) corrupt(backendName, key string, replacement []byte) {
	h.t.Helper()
	be, ok := h.backends[backendName]
	if !ok {
		h.t.Fatalf("corrupt: %q is not in this harness fleet", backendName)
	}
	if _, err := be.PutObject(context.Background(), internalKey(key),
		bytes.NewReader(replacement), int64(len(replacement)), "application/octet-stream", nil); err != nil {
		h.t.Fatalf("corrupting copy on %s: %v", backendName, err)
	}
}

// encode returns body in the stored form this harness writes, so a test can
// plant bytes a backend would plausibly hold rather than obvious garbage.
func (h *harness) encode(body []byte) []byte {
	h.t.Helper()
	if h.codec == nil {
		return body
	}
	var buf bytes.Buffer
	if _, err := h.codec.Compress(&buf, bytes.NewReader(body)); err != nil {
		h.t.Fatalf("encoding %d bytes: %v", len(body), err)
	}
	return buf.Bytes()
}

// storedBytes reads a copy's bytes straight off its backend, exactly as stored.
func (h *harness) storedBytes(backendName, key string) []byte {
	h.t.Helper()
	be, ok := h.backends[backendName]
	if !ok {
		h.t.Fatalf("storedBytes: %q is not in this harness fleet", backendName)
	}
	result, err := be.GetObject(context.Background(), internalKey(key), "")
	if err != nil {
		h.t.Fatalf("direct read of %s from %s: %v", key, backendName, err)
	}
	defer result.Body.Close()
	stored, err := io.ReadAll(result.Body)
	if err != nil {
		h.t.Fatalf("reading stored bytes of %s: %v", key, err)
	}
	return stored
}

// decodeStored reads a copy off its backend and decodes it, returning the bytes
// the client wrote. This is byte verification that owes nothing to the proxy: a
// read through the proxy would exercise the same decode path a move might have
// broken, so a copy is checked by decoding it here instead.
func (h *harness) decodeStored(backendName, key string) []byte {
	h.t.Helper()
	stored := h.storedBytes(backendName, key)
	if h.codec == nil {
		return stored
	}
	reader, err := h.codec.Decompress(bytes.NewReader(stored))
	if err != nil {
		h.t.Fatalf("decoding stored copy of %s on %s: %v", key, backendName, err)
	}
	defer reader.Close()
	plain, err := io.ReadAll(reader)
	if err != nil {
		h.t.Fatalf("reading decoded copy of %s on %s: %v", key, backendName, err)
	}
	return plain
}

// assertEveryCopyDecodesTo requires every copy the ledger claims for key to
// decode to want, and its stored length to match what the ledger recorded.
//
// Reading each copy directly is the point: a GET through the proxy fails over
// to a healthy replica, so one truncated or mis-encoded copy is invisible from
// the client side, which is exactly the state a move can leave behind. stage
// names the operation under test so a failure says which step lost the bytes.
func (h *harness) assertEveryCopyDecodesTo(key string, want []byte, stage string) {
	h.t.Helper()
	backends := h.objectBackends(key)
	if len(backends) == 0 {
		h.t.Fatalf("%s: ledger lists no copies of %q", stage, key)
	}
	for _, name := range backends {
		if got := h.decodeStored(name, key); !bytes.Equal(got, want) {
			h.t.Errorf("%s: copy of %s on %s decoded to %d bytes, want %d",
				stage, key, name, len(got), len(want))
		}
		if recorded, physical := h.storedSize(key), h.backendSize(name, key); recorded != physical {
			h.t.Errorf("%s: ledger records %d bytes for %s on %s, backend holds %d",
				stage, recorded, key, name, physical)
		}
	}
}
