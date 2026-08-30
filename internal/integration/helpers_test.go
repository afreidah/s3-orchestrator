// -------------------------------------------------------------------------------
// Integration Test Helpers
//
// Author: Alex Freidah
//
// Shared setup and teardown utilities for integration tests. Uses testcontainers
// to spin up PostgreSQL, MinIO (x3), and Redis containers automatically. No
// external docker-compose required  -  just `go test -tags integration`.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/proxy/reconcile"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/postgres"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// virtualBucket is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
const virtualBucket = "test-bucket"

// proxyAddr and related package-level variables used by this package.
//
// testDBConfig and minioEndpoints describe the containers rather than the
// shared fixture built on them, so a harness can provision its own database and
// its own buckets alongside it. See harness_test.go.
var (
	proxyAddr         string
	testDBConfig      config.DatabaseConfig
	minioEndpoints    []string
	testDB            *sql.DB
	testStack         *proxytest.Stack
	testReconciler    *reconcile.Manager
	testCoord         *writepath.Coordinator
	testWorkers       *proxytest.Workers
	testStore         *postgres.Store
	testFailableStore *FailableStore
	testDatabaseCB    *breaker.CircuitBreaker
	testBackends      map[string]s3be.ObjectBackend
	testBackendOrder  []string
	allBackends       map[string]s3be.ObjectBackend
	allBackendOrder   []string
)

// minioInstance holds a running MinIO container and its connection details.
type minioInstance struct {
	container *tcminio.MinioContainer
	endpoint  string
	bucket    string
}

// mustStartPostgres launches the Postgres testcontainer used by every
// integration test. Bails the process on any failure because there is
// no useful test run without a database.
func mustStartPostgres(ctx context.Context) *tcpostgres.PostgresContainer {
	// Debian rather than alpine, matching the store package's fixture: musl
	// has no locale data, so text sorts by byte there whatever collation a
	// query asks for, and an ordering regression would go unnoticed.
	c, err := tcpostgres.Run(ctx,
		"postgres:16",
		tcpostgres.WithDatabase("s3proxy_test"),
		tcpostgres.WithUsername("s3proxy"),
		tcpostgres.WithPassword("s3proxy"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(30*time.Second)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start postgres: %v\n", err)
		os.Exit(1)
	}
	return c
}

// mustStartMinios launches the three MinIO testcontainers the suite
// uses to model a multi-backend fleet, sets MINIO{N}_ENDPOINT env vars
// for downstream config, and bails the process on any failure.
func mustStartMinios(ctx context.Context) []minioInstance {
	specs := []struct {
		name   string
		envKey string
		bucket string
	}{
		{"minio-1", "MINIO1_ENDPOINT", "backend1"},
		{"minio-2", "MINIO2_ENDPOINT", "backend2"},
		{"minio-3", "MINIO3_ENDPOINT", "backend3"},
	}
	minios := make([]minioInstance, len(specs))
	for i, spec := range specs {
		ctr, err := tcminio.Run(ctx, "minio/minio:latest")
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to start %s: %v\n", spec.name, err)
			os.Exit(1)
		}
		endpoint, err := ctr.ConnectionString(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get %s endpoint: %v\n", spec.name, err)
			os.Exit(1)
		}
		minios[i] = minioInstance{
			container: ctr,
			endpoint:  "http://" + endpoint,
			bucket:    spec.bucket,
		}
		os.Setenv(spec.envKey, minios[i].endpoint)
		minioEndpoints = append(minioEndpoints, minios[i].endpoint)
	}
	return minios
}

// mustStartRedis launches the Redis testcontainer and exports
// REDIS_ADDR for downstream config consumption.
func mustStartRedis(ctx context.Context) *tcredis.RedisContainer {
	c, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start redis: %v\n", err)
		os.Exit(1)
	}
	connStr, err := c.ConnectionString(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get redis endpoint: %v\n", err)
		os.Exit(1)
	}
	addr := strings.TrimPrefix(connStr, "redis://")
	addr = strings.TrimSuffix(addr, "/0")
	os.Setenv("REDIS_ADDR", addr)
	return c
}

// mustCreateBuckets pre-creates each backend's bucket on the MinIO
// container that hosts it; the proxy assumes the bucket already exists
// when it routes a write.
func mustCreateBuckets(ctx context.Context, minios []minioInstance) {
	for _, mi := range minios {
		mc := s3.New(s3.Options{
			BaseEndpoint: aws.String(mi.endpoint),
			Region:       "us-east-1",
			Credentials:  credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", ""),
			UsePathStyle: true,
		})
		if _, err := mc.CreateBucket(ctx, &s3.CreateBucketInput{
			Bucket: aws.String(mi.bucket),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "failed to create bucket %s: %v\n", mi.bucket, err)
			os.Exit(1)
		}
	}
}

// mustPostgresHostPort resolves the Postgres container's externally
// reachable host and port. Bails the process on either lookup failing.
func mustPostgresHostPort(ctx context.Context, c *tcpostgres.PostgresContainer) (string, int) {
	host, err := c.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get postgres host: %v\n", err)
		os.Exit(1)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get postgres port: %v\n", err)
		os.Exit(1)
	}
	return host, int(port.Num())
}

// TestMain is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func TestMain(m *testing.M) {
	// Silence the proxy's request logger so test output is clean.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()

	pgContainer := mustStartPostgres(ctx)
	minios := mustStartMinios(ctx)
	redisContainer := mustStartRedis(ctx)
	mustCreateBuckets(ctx, minios)

	pgHost, pgPort := mustPostgresHostPort(ctx, pgContainer)

	// ---------------------------------------------------------------
	// Build config and wire up components
	// ---------------------------------------------------------------

	cfg := &config.Config{
		Server: config.ServerConfig{
			ListenAddr: "127.0.0.1:0",
		},
		Buckets: []config.BucketConfig{
			{
				Name: virtualBucket,
				Credentials: []config.CredentialConfig{
					{
						AccessKeyID:     "test",
						SecretAccessKey: "test",
					},
				},
			},
		},
		Database: config.DatabaseConfig{
			Host:     pgHost,
			Port:     pgPort,
			Database: "s3proxy_test",
			User:     "s3proxy",
			Password: "s3proxy",
			SSLMode:  "disable",
		},
		CircuitBreaker: config.CircuitBreakerConfig{
			FailureThreshold: 3,
			OpenTimeout:      500 * time.Millisecond,
			CacheTTL:         60 * time.Second,
		},
		Backends: []config.BackendConfig{
			{
				Name:            "minio-1",
				Endpoint:        minios[0].endpoint,
				Region:          "us-east-1",
				Bucket:          "backend1",
				AccessKeyID:     "minioadmin",
				SecretAccessKey: "minioadmin",
				ForcePathStyle:  true,
				QuotaBytes:      1024,
			},
			{
				Name:            "minio-2",
				Endpoint:        minios[1].endpoint,
				Region:          "us-east-1",
				Bucket:          "backend2",
				AccessKeyID:     "minioadmin",
				SecretAccessKey: "minioadmin",
				ForcePathStyle:  true,
				QuotaBytes:      2048,
			},
			{
				Name:            "minio-3",
				Endpoint:        minios[2].endpoint,
				Region:          "us-east-1",
				Bucket:          "backend3",
				AccessKeyID:     "minioadmin",
				SecretAccessKey: "minioadmin",
				ForcePathStyle:  true,
				QuotaBytes:      2048,
			},
		},
	}

	if err := cfg.SetDefaultsAndValidate(); err != nil {
		fmt.Fprintf(os.Stderr, "config validation failed: %v\n", err)
		os.Exit(1)
	}

	testDBConfig = cfg.Database

	dbCB := store.NewDatabaseBreaker(cfg.CircuitBreaker)
	testDatabaseCB = dbCB

	db, err := postgres.NewStore(ctx, &cfg.Database, dbCB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create store: %v\n", err)
		os.Exit(1)
	}

	if err := db.RunMigrations(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run migrations: %v\n", err)
		os.Exit(1)
	}

	if err := db.SyncQuotaLimits(ctx, cfg.Backends); err != nil {
		fmt.Fprintf(os.Stderr, "failed to sync quota limits: %v\n", err)
		os.Exit(1)
	}

	backends := make(map[string]s3be.ObjectBackend)
	var backendOrder []string
	for _, bcfg := range cfg.Backends {
		b, err := s3be.NewS3Backend(context.Background(), &bcfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create backend %s: %v\n", bcfg.Name, err)
			os.Exit(1)
		}
		backends[bcfg.Name] = b
		backendOrder = append(backendOrder, bcfg.Name)
	}

	testStore = db
	// Keep all backends available for tests that need 3+ backends.
	allBackends = backends
	allBackendOrder = backendOrder
	// Default test manager uses only the first 2 backends to preserve
	// existing spread/rebalance test math.
	testBackends = make(map[string]s3be.ObjectBackend)
	for _, name := range backendOrder[:2] {
		testBackends[name] = backends[name]
	}
	testBackendOrder = backendOrder[:2]

	// Wire: postgres store (CB at DBTX level) -> FailableStore -> manager
	failableStore := newFailableStore(db)
	testFailableStore = failableStore

	stores := newStores(failableStore)

	stack := proxytest.Build(stores, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{
			Backends:        testBackends,
			Order:           testBackendOrder,
			BackendTimeout:  30 * time.Second,
			RoutingStrategy: config.RoutingPack,
			Metrics:         newMetricsAdapter(failableStore),
		}),
		PendingEnabled: true,
		CacheTTL:       60 * time.Second,
		BackendTimeout: 30 * time.Second,
	})
	workers := proxytest.BuildWorkers(stack, stores)
	testStack = stack
	testCoord = writepath.New(stack.Runtime, db, false)
	testReconciler = reconcile.NewManager(&reconcile.Deps{
		Backends: stack.Runtime, Stores: db, Usage: stack.Runtime.Acct(),
	})
	testWorkers = workers

	srv := &s3api.Server{
		Objects:   stack.Objects,
		Multipart: stack.Multipart,
	}
	bucketAuth, err := auth.NewBucketRegistry(cfg.Buckets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build bucket registry: %v\n", err)
		os.Exit(1)
	}
	srv.SetBucketAuth(bucketAuth)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to listen: %v\n", err)
		os.Exit(1)
	}
	proxyAddr = listener.Addr().String()

	httpServer := &http.Server{
		Handler:      srv,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
	}
	go httpServer.Serve(listener)

	connStr := cfg.Database.ConnectionString()
	testDB, err = sql.Open("pgx", connStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open test db: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	// ---------------------------------------------------------------
	// Cleanup
	// ---------------------------------------------------------------

	httpServer.Shutdown(ctx)
	testDB.Close()
	db.Close()

	// Terminate containers (best-effort, testcontainers handles cleanup
	// via Ryuk even if these fail).
	pgContainer.Terminate(ctx)
	for _, mi := range minios {
		mi.container.Terminate(ctx)
	}
	redisContainer.Terminate(ctx)

	os.Exit(code)
}

// newS3Client returns an AWS SDK v2 S3 client pointed at the in-process proxy.
func newS3Client(t *testing.T) *s3.Client {
	t.Helper()
	return s3.New(s3.Options{
		BaseEndpoint: aws.String("http://" + proxyAddr),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
	})
}

// newResilientS3Client is newS3Client with a longer retry budget. The shared
// MinIO container intermittently returns a transient 502 under concurrent load
// on CI runners; the standard 3 attempts can all land inside that window. Used
// by stress scenarios where a momentary backend hiccup is infra noise rather
// than the invariant under test (a persistent 5xx still fails after retries).
func newResilientS3Client(t *testing.T) *s3.Client {
	t.Helper()
	return s3.New(s3.Options{
		BaseEndpoint: aws.String("http://" + proxyAddr),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
		Retryer: retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = 10
			o.MaxBackoff = 1 * time.Second
		}),
	})
}

// internalKey returns the bucket-prefixed key as stored in the DB and backends.
func internalKey(key string) string {
	return virtualBucket + "/" + key
}

// queryObjectBackend returns which backend stores the given object key.
// Automatically prefixes the key with the virtual bucket name.
func queryObjectBackend(t *testing.T, key string) string {
	t.Helper()
	var backendName string
	err := testDB.QueryRow("SELECT backend_name FROM object_locations WHERE object_key = $1", internalKey(key)).Scan(&backendName)
	if err != nil {
		t.Fatalf("queryObjectBackend(%q): %v", key, err)
	}
	return backendName
}

// queryQuotaUsed returns the bytes_used value for a backend.
func queryQuotaUsed(t *testing.T, backendName string) int64 {
	t.Helper()
	var bytesUsed int64
	err := testDB.QueryRow("SELECT bytes_used FROM backend_quotas WHERE backend_name = $1", backendName).Scan(&bytesUsed)
	if err != nil {
		t.Fatalf("queryQuotaUsed(%q): %v", backendName, err)
	}
	return bytesUsed
}

// queryStoredSize returns size_bytes for a key: what the backend actually
// holds, which for an encoded object is smaller than what the client wrote.
func queryStoredSize(t *testing.T, key string) int64 {
	t.Helper()
	var size int64
	err := testDB.QueryRow(
		"SELECT size_bytes FROM object_locations WHERE object_key = $1",
		virtualBucket+"/"+key).Scan(&size)
	if err != nil {
		t.Fatalf("queryStoredSize(%q): %v", key, err)
	}
	return size
}

// queryCompressionAlgorithm returns the encoding a key is stored in, or "" for
// a copy held verbatim. Tests that mean to exercise the compressed path assert
// on this: an object the ratio floor declined is stored plain, and every
// compressed-read assertion would then pass without the encoded path running.
func queryCompressionAlgorithm(t *testing.T, key string) string {
	t.Helper()
	var algorithm sql.NullString
	err := testDB.QueryRow(
		"SELECT compression_algorithm FROM object_locations WHERE object_key = $1",
		virtualBucket+"/"+key).Scan(&algorithm)
	if err != nil {
		t.Fatalf("queryCompressionAlgorithm(%q): %v", key, err)
	}
	return algorithm.String
}

// queryLogicalSize returns logical_size for a key: the size the client wrote
// and the size the object is known by, whatever form it is stored in.
func queryLogicalSize(t *testing.T, key string) int64 {
	t.Helper()
	var size sql.NullInt64
	err := testDB.QueryRow(
		"SELECT logical_size FROM object_locations WHERE object_key = $1",
		virtualBucket+"/"+key).Scan(&size)
	if err != nil {
		t.Fatalf("queryLogicalSize(%q): %v", key, err)
	}
	return size.Int64
}

// backendObjectSize reports how many bytes a backend physically holds for an
// object key. Every other size in the system - the ledger's size_bytes, the
// quota, the usage counters - is a derived number that can drift from this one,
// so accounting assertions measure against it rather than against each other.
func backendObjectSize(t *testing.T, backendName, key string) int64 {
	t.Helper()
	return backendRawObjectSize(t, backendName, internalKey(key))
}

// backendRawObjectSize is backendObjectSize for a key the orchestrator stores
// outside a virtual bucket, which multipart part objects are.
func backendRawObjectSize(t *testing.T, backendName, storedKey string) int64 {
	t.Helper()
	be, ok := allBackends[backendName]
	if !ok {
		t.Fatalf("backendRawObjectSize: backend %q is not configured", backendName)
	}
	head, err := be.HeadObject(context.Background(), storedKey)
	if err != nil {
		t.Fatalf("backendRawObjectSize(%s, %s): %v", backendName, storedKey, err)
	}
	return head.Size
}

// queryMultipartBackend returns the backend an in-flight upload is pinned to.
func queryMultipartBackend(t *testing.T, uploadID string) string {
	t.Helper()
	var backendName string
	err := testDB.QueryRow(
		"SELECT backend_name FROM multipart_uploads WHERE upload_id = $1", uploadID).Scan(&backendName)
	if err != nil {
		t.Fatalf("queryMultipartBackend(%q): %v", uploadID, err)
	}
	return backendName
}

// setQuotaLimits gives every backend the same capacity for the duration of one
// test and puts the configured values back afterwards.
//
// The fleet is provisioned at one and two kilobytes, which is what the spread
// and rebalance tests do their arithmetic against. A test needing room for a
// multi-chunk object, or needing a limit pitched at one exact size, has to move
// them, and leaving them moved would quietly change the placement decisions
// every later test asserts on.
func setQuotaLimits(t *testing.T, limit int64) {
	t.Helper()
	original := map[string]int64{}
	rows, err := testDB.Query("SELECT backend_name, bytes_limit FROM backend_quotas")
	if err != nil {
		t.Fatalf("read quota limits: %v", err)
	}
	for rows.Next() {
		var name string
		var value int64
		if err := rows.Scan(&name, &value); err != nil {
			rows.Close()
			t.Fatalf("scan quota limit: %v", err)
		}
		original[name] = value
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read quota limits: %v", err)
	}

	if _, err := testDB.Exec("UPDATE backend_quotas SET bytes_limit = $1", limit); err != nil {
		t.Fatalf("set quota limits: %v", err)
	}
	t.Cleanup(func() {
		for name, value := range original {
			if _, err := testDB.Exec(
				"UPDATE backend_quotas SET bytes_limit = $1 WHERE backend_name = $2", value, name); err != nil {
				t.Errorf("restore quota limit for %s: %v", name, err)
			}
		}
	})
}

// resetState truncates all object/multipart tables and re-establishes the
// backend_quotas row for every configured backend with usage/orphans
// zeroed. The re-sync is necessary because tests that drain a backend
// (TestOverReplicationDrainingBackendRemovedFirst, TestDrainBackend,
// TestDrainBackend_WriteExclusion) leave its quota row deleted via
// runDrain -> DeleteBackendData; subsequent tests that build a manager
// referencing all three backends would then hit FK violations on insert.
func resetState(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		"DELETE FROM cleanup_queue",
		"DELETE FROM pending_objects",
		"DELETE FROM multipart_parts",
		"DELETE FROM multipart_uploads",
		"DELETE FROM object_locations",
	} {
		if _, err := testDB.Exec(q); err != nil {
			t.Fatalf("resetState: %v", err)
		}
	}
	if err := testStore.SyncQuotaLimits(context.Background(), []config.BackendConfig{
		{Name: "minio-1", QuotaBytes: 1024},
		{Name: "minio-2", QuotaBytes: 2048},
		{Name: "minio-3", QuotaBytes: 2048},
	}); err != nil {
		t.Fatalf("resetState: SyncQuotaLimits: %v", err)
	}
	if _, err := testDB.Exec("UPDATE backend_quotas SET bytes_used = 0, orphan_bytes = 0, updated_at = NOW()"); err != nil {
		t.Fatalf("resetState: %v", err)
	}
	testStack.Objects.LocationCache().Clear()
	testStack.Drain.ClearState()
}

// uniqueKey generates a collision-free object key.
func uniqueKey(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s/%s-%d", prefix, t.Name(), time.Now().UnixNano())
}

// queryObjectCopies returns the number of copies (rows) for the given object key.
// Automatically prefixes the key with the virtual bucket name.
func queryObjectCopies(t *testing.T, key string) int {
	t.Helper()
	var count int
	err := testDB.QueryRow(
		"SELECT COUNT(*) FROM object_locations WHERE object_key = $1", internalKey(key),
	).Scan(&count)
	if err != nil {
		t.Fatalf("queryObjectCopies(%q): %v", key, err)
	}
	return count
}

// queryObjectBackends returns all backend names storing copies of the given key.
// Automatically prefixes the key with the virtual bucket name.
func queryObjectBackends(t *testing.T, key string) []string {
	t.Helper()
	rows, err := testDB.Query(
		"SELECT backend_name FROM object_locations WHERE object_key = $1 ORDER BY created_at ASC", internalKey(key),
	)
	if err != nil {
		t.Fatalf("queryObjectBackends(%q): %v", key, err)
	}
	defer rows.Close()

	var backends []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("queryObjectBackends scan: %v", err)
		}
		backends = append(backends, name)
	}
	return backends
}

// queryOrphanBytes returns the orphan_bytes value for a backend.
func queryOrphanBytes(t *testing.T, backendName string) int64 {
	t.Helper()
	var orphanBytes int64
	err := testDB.QueryRow("SELECT orphan_bytes FROM backend_quotas WHERE backend_name = $1", backendName).Scan(&orphanBytes)
	if err != nil {
		t.Fatalf("queryOrphanBytes(%q): %v", backendName, err)
	}
	return orphanBytes
}

// queryCleanupQueueCount returns the number of items in cleanup_queue for a backend.
func queryCleanupQueueCount(t *testing.T, backendName string) int {
	t.Helper()
	var count int
	err := testDB.QueryRow("SELECT COUNT(*) FROM cleanup_queue WHERE backend_name = $1", backendName).Scan(&count)
	if err != nil {
		t.Fatalf("queryCleanupQueueCount(%q): %v", backendName, err)
	}
	return count
}

// queryCleanupQueueItem returns the first cleanup_queue item for a backend.
func queryCleanupQueueItem(t *testing.T, backendName string) (objectKey string, sizeBytes int64, attempts int32) {
	t.Helper()
	err := testDB.QueryRow(
		"SELECT object_key, size_bytes, attempts FROM cleanup_queue WHERE backend_name = $1 ORDER BY created_at LIMIT 1",
		backendName,
	).Scan(&objectKey, &sizeBytes, &attempts)
	if err != nil {
		t.Fatalf("queryCleanupQueueItem(%q): %v", backendName, err)
	}
	return
}

// setOrphanBytes directly sets orphan_bytes for a backend via SQL (for test setup).
func setOrphanBytes(t *testing.T, backendName string, amount int64) {
	t.Helper()
	_, err := testDB.Exec("UPDATE backend_quotas SET orphan_bytes = $1, updated_at = NOW() WHERE backend_name = $2", amount, backendName)
	if err != nil {
		t.Fatalf("setOrphanBytes(%q, %d): %v", backendName, amount, err)
	}
}

// newThreeBackendStack creates a proxy stack with all 3 backends for
// tests that need more than 2 backends (e.g., over-replication with factor=3).
// Returns the stack and its fully-wired worker bundle so callers that need
// a specific worker (Replicator/OverReplicationCleaner/...) can reach it
// directly.
func newThreeBackendStack(t *testing.T) (*proxytest.Stack, *proxytest.Workers) {
	t.Helper()
	stores := newStores(testFailableStore)
	st := proxytest.New(t, stores, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{
			Backends:        allBackends,
			Order:           allBackendOrder,
			BackendTimeout:  30 * time.Second,
			RoutingStrategy: config.RoutingPack,
			Metrics:         newMetricsAdapter(testFailableStore),
		}),
		CacheTTL:       60 * time.Second,
		BackendTimeout: 30 * time.Second,
	})
	return st, proxytest.BuildWorkers(st, stores)
}

// newStores returns src typed as the wide metadata-store contract every
// proxy consumer depends on. Identity at the type level - kept so the
// call sites read uniformly with the production DI wiring.
func newStores(src storetest.MetadataStore) storetest.MetadataStore { return src }

// roleStore names the role union tests need on a single source value.
type roleStore interface {
	core.ObjectStore
	core.QuotaStore
	core.MultipartStore
	core.ReplicationStore
	core.CleanupStore
	core.PendingStore
	core.IntegrityStore
	core.ExpiredObjectsLister
	core.BackendLifecycleStore
	core.DashboardStore
	core.UsageFlusher
	core.AdvisoryLocker
}

// metricsAdapter carries the CB-wrapped role views MetricsCollector needs.
type metricsAdapter struct {
	core.DashboardStore
	core.ReplicationStore
}

// newMetricsAdapter returns a proxy.MetricsDeps-compatible value backed
// by src; CB protection lives in the underlying driver.
func newMetricsAdapter(src roleStore) *metricsAdapter {
	return &metricsAdapter{
		DashboardStore:   src,
		ReplicationStore: src,
	}
}

// envOrDefault is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// -------------------------------------------------------------------------
// FailableStore  -  injectable failure wrapper for circuit breaker tests
// -------------------------------------------------------------------------

// errSimulatedDBOutage is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
// errSimulatedDBOutage wraps core.ErrDBUnavailable so the proxy's
// degraded-mode logic - which keys off ErrDBUnavailable bubbling up
// from the store - reacts identically to a real breaker-open response.
// FailableStore.SetFailing(true) returns this error from every role
// method so callers see the same shape as a tripped CB without having
// to actually trip the breaker.
var errSimulatedDBOutage = fmt.Errorf("simulated database outage: %w", core.ErrDBUnavailable)

// errSimulatedCommitFailure is the one-shot mid-PUT commit failure
// FailableStore.SetFailCommitOnce arms. Distinct from errSimulatedDBOutage
// because the test wants the PUT to fail immediately, not be treated as
// a degraded-mode signal that triggers fallback paths.
var errSimulatedCommitFailure = errors.New("simulated commit failure")

// FailableStore wraps a concrete metadata store and can be toggled to return
// connection errors, simulating a database outage for circuit breaker
// integration tests. It embeds every narrow role interface so one instance
// satisfies any role the proxy asks for; a single *postgres.Store is assigned
// to is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
type FailableStore struct {
	storetest.MetadataStore // embedded inner satisfies every method by default
	inner                   *postgres.Store
	mu                      sync.Mutex
	failing                 bool
	failCommitOnce          bool // when true, RecordObjectAndClearPending fails once, then auto-clears
}

// newFailableStore returns a FailableStore whose role views all resolve to
// the same concrete *postgres.Store.
func newFailableStore(db *postgres.Store) *FailableStore {
	return &FailableStore{MetadataStore: db, inner: db}
}

// SetFailing is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) SetFailing(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = v
}

// isFailing is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) isFailing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failing
}

// GetAllObjectLocations is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.GetAllObjectLocations(ctx, key)
}

// RecordObject is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
// A commit carrying a pending intent also honours the one-shot fail-commit
// flag: sustained outages surface as errSimulatedDBOutage (wraps
// ErrDBUnavailable, triggers degraded-mode fallbacks), while the one-shot blip
// surfaces as errSimulatedCommitFailure (plain) so the caller fails the PUT
// instead of reading the error as a degraded-mode signal.
func (f *FailableStore) RecordObject(ctx context.Context, req *core.RecordObjectRequest) ([]core.DeletedCopy, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	if req.IntentID != "" && f.consumeFailCommitOnce() {
		return nil, errSimulatedCommitFailure
	}
	return f.inner.RecordObject(ctx, req)
}

// SetFailCommitOnce arms a one-shot failure on RecordObjectAndClearPending
// so the next PUT lands its bytes on the backend, inserts a pending intent,
// and then sees the metadata commit fail  -  exactly the data-loss scenario
// the pending-row pattern exists to recover from.
func (f *FailableStore) SetFailCommitOnce() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCommitOnce = true
}

// consumeFailCommitOnce returns true and clears the one-shot flag if it
// was armed; otherwise returns false. The auto-clear ensures the next PUT
// in the same test sees the original behaviour.
func (f *FailableStore) consumeFailCommitOnce() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCommitOnce {
		f.failCommitOnce = false
		return true
	}
	return false
}

// DeleteObject is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) DeleteObject(ctx context.Context, key string) ([]core.DeletedCopy, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.DeleteObject(ctx, key)
}

// ListObjects is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.ListObjectsResult, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.ListObjects(ctx, prefix, startAfter, maxKeys)
}

// GetBackendWithSpace is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error) {
	if f.isFailing() {
		return "", errSimulatedDBOutage
	}
	return f.inner.GetBackendWithSpace(ctx, size, backendOrder)
}

// GetLeastUtilizedBackend is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error) {
	if f.isFailing() {
		return "", errSimulatedDBOutage
	}
	return f.inner.GetLeastUtilizedBackend(ctx, size, eligible)
}

// CreateMultipartUpload is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) CreateMultipartUpload(ctx context.Context, params *core.CreateMultipartUploadParams) error {
	if f.isFailing() {
		return errSimulatedDBOutage
	}
	return f.inner.CreateMultipartUpload(ctx, params)
}

// GetMultipartUpload is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) GetMultipartUpload(ctx context.Context, uploadID string) (*core.MultipartUpload, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.GetMultipartUpload(ctx, uploadID)
}

// RecordPart is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, form *core.StoredForm) error {
	if f.isFailing() {
		return errSimulatedDBOutage
	}
	return f.inner.RecordPart(ctx, uploadID, partNumber, etag, size, form)
}

// GetParts is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) GetParts(ctx context.Context, uploadID string) ([]core.MultipartPart, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.GetParts(ctx, uploadID)
}

// DeleteMultipartUpload is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) DeleteMultipartUpload(ctx context.Context, uploadID string) error {
	if f.isFailing() {
		return errSimulatedDBOutage
	}
	return f.inner.DeleteMultipartUpload(ctx, uploadID)
}

// GetQuotaStats is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) GetQuotaStats(ctx context.Context) (map[string]core.QuotaStat, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.GetQuotaStats(ctx)
}

// GetObjectCounts is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) GetObjectCounts(ctx context.Context) (map[string]int64, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.GetObjectCounts(ctx)
}

// GetActiveMultipartCounts is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.GetActiveMultipartCounts(ctx)
}

// GetStaleMultipartUploads is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]core.MultipartUpload, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.GetStaleMultipartUploads(ctx, olderThan)
}

// ListDirectoryChildren is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*core.DirectoryListResult, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.ListDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}

// ListObjectsByBackend is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]core.ObjectLocation, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.ListObjectsByBackend(ctx, backendName, limit)
}

// MoveObjectLocation is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error) {
	if f.isFailing() {
		return 0, errSimulatedDBOutage
	}
	return f.inner.MoveObjectLocation(ctx, key, fromBackend, toBackend)
}

// GetUnderReplicatedObjects is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.GetUnderReplicatedObjects(ctx, factor, limit)
}

// RecordReplica is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string) (int64, bool, error) {
	if f.isFailing() {
		return 0, false, errSimulatedDBOutage
	}
	return f.inner.RecordReplica(ctx, key, targetBackend, sourceBackend)
}

// GetOverReplicatedObjects is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]core.ObjectLocation, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.GetOverReplicatedObjects(ctx, factor, limit)
}

// CountOverReplicatedObjects is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error) {
	if f.isFailing() {
		return 0, errSimulatedDBOutage
	}
	return f.inner.CountOverReplicatedObjects(ctx, factor)
}

// RemoveExcessCopy is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
func (f *FailableStore) RemoveExcessCopy(ctx context.Context, key, backendName string, factor int) (bool, error) {
	if f.isFailing() {
		return false, errSimulatedDBOutage
	}
	return f.inner.RemoveExcessCopy(ctx, key, backendName, factor)
}

// tripCircuitBreaker drives the test breaker through enough simulated
// DB failures to trip it open. With CB now living at the driver's DBTX
// chokepoint, FailableStore-induced role-level errors no longer reach
// the breaker; tests that want the breaker open call PostCheck directly.
func tripCircuitBreaker(t *testing.T) {
	t.Helper()
	for testDatabaseCB.IsHealthy() {
		_ = testDatabaseCB.PostCheck(errors.New("simulated DB outage"))
	}
}

// waitForRecovery waits for the circuit to probe and close after the open timeout.
// Polls until the circuit is healthy or the timeout expires.
func waitForRecovery(t *testing.T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("circuit breaker did not recover within 5s")
			return
		default:
			// Wait at least the open timeout (500ms) before probing
			time.Sleep(600 * time.Millisecond)
			// Make a request to trigger the half-open probe
			client := newS3Client(t)
			client.HeadObject(context.Background(), &s3.HeadObjectInput{
				Bucket: aws.String(virtualBucket),
				Key:    aws.String("probe-recovery"),
			})
			if testDatabaseCB.IsHealthy() {
				return
			}
		}
	}
}

// newTestS3Backend creates an S3Backend for a test MinIO instance, avoiding
// duplicate endpoint/credential wiring across tests.
func newTestS3Backend(t *testing.T, name string) *s3be.S3Backend {
	t.Helper()

	cfgs := map[string]config.BackendConfig{
		"minio-1": {
			Name:            "minio-1",
			Endpoint:        envOrDefault("MINIO1_ENDPOINT", "http://localhost:19000"),
			Region:          "us-east-1",
			Bucket:          "backend1",
			AccessKeyID:     "minioadmin",
			SecretAccessKey: "minioadmin",
			ForcePathStyle:  true,
		},
		"minio-2": {
			Name:            "minio-2",
			Endpoint:        envOrDefault("MINIO2_ENDPOINT", "http://localhost:19002"),
			Region:          "us-east-1",
			Bucket:          "backend2",
			AccessKeyID:     "minioadmin",
			SecretAccessKey: "minioadmin",
			ForcePathStyle:  true,
		},
		"minio-3": {
			Name:            "minio-3",
			Endpoint:        envOrDefault("MINIO3_ENDPOINT", "http://localhost:19004"),
			Region:          "us-east-1",
			Bucket:          "backend3",
			AccessKeyID:     "minioadmin",
			SecretAccessKey: "minioadmin",
			ForcePathStyle:  true,
		},
	}

	cfg, ok := cfgs[name]
	if !ok {
		t.Fatalf("unknown backend %q", name)
	}

	backend, err := s3be.NewS3Backend(context.Background(), &cfg)
	if err != nil {
		t.Fatalf("NewS3Backend(%s): %v", name, err)
	}
	return backend
}

// mustBucketRegistry builds a registry from config the test controls, failing
// the test if that config turns out to be ambiguous.
func mustBucketRegistry(tb testing.TB, buckets []config.BucketConfig) *auth.BucketRegistry {
	tb.Helper()
	br, err := auth.NewBucketRegistry(buckets)
	if err != nil {
		tb.Fatalf("NewBucketRegistry: %v", err)
	}
	return br
}
