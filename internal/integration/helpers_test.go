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
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/postgres"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// virtualBucket is an integration-test fixture helper; see file header for
// the surrounding lifecycle the helpers participate in.
const virtualBucket = "test-bucket"

// proxyAddr and related package-level variables used by this package.
var (
	proxyAddr         string
	testDB            *sql.DB
	testManager       *proxy.BackendManager
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
	c, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
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

	manager := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        testBackends,
		Stores:          stores,
		PendingEnabled:  true,
		Dashboard:       failableStore,
		Metrics:         newMetricsAdapter(failableStore),
		Order:           testBackendOrder,
		CacheTTL:        60 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := proxytest.BuildWorkers(manager, stores)
	testManager = manager
	testWorkers = workers

	srv := &s3api.Server{
		Manager: manager,
	}
	srv.SetBucketAuth(auth.NewBucketRegistry(cfg.Buckets))

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
	testManager.ClearCache()
	testManager.ClearDrainState()
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

// newThreeBackendManager creates a BackendManager with all 3 backends for
// tests that need more than 2 backends (e.g., over-replication with factor=3).
// Returns the manager and its fully-wired worker bundle so callers that need
// a specific worker (Replicator/OverReplicationCleaner/...) can reach it
// directly, since these are no longer fields on BackendManager.
func newThreeBackendManager(t *testing.T) (*proxy.BackendManager, *proxytest.Workers) {
	t.Helper()
	stores := newStores(testFailableStore)
	mgr := proxytest.NewManager(t, &proxy.BackendManagerConfig{
		Backends:        allBackends,
		Stores:          stores,
		Dashboard:       testFailableStore,
		Metrics:         newMetricsAdapter(testFailableStore),
		Order:           allBackendOrder,
		CacheTTL:        60 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	workers := proxytest.BuildWorkers(mgr, stores)
	return mgr, workers
}

// newStores returns src typed as the wide metadata-store contract every
// proxy consumer depends on. Identity at the type level - kept so the
// call sites read uniformly with the production DI wiring.
func newStores(src core.MetadataStore) core.MetadataStore { return src }

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
	core.MetadataStore // embedded inner satisfies every method by default
	inner              *postgres.Store
	mu                 sync.Mutex
	failing            bool
	failCommitOnce     bool // when true, RecordObjectAndClearPending fails once, then auto-clears
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
func (f *FailableStore) RecordObject(ctx context.Context, key, backend string, size int64, enc *core.EncryptionMeta) ([]core.DeletedCopy, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	return f.inner.RecordObject(ctx, key, backend, size, enc)
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

// RecordObjectAndClearPending honours both the global failing flag and
// the one-shot fail-commit flag. Sustained outages surface as
// errSimulatedDBOutage (wraps ErrDBUnavailable, triggers degraded-mode
// fallbacks); the one-shot blip surfaces as errSimulatedCommitFailure
// (plain) so the caller fails the PUT instead of treating the error as
// a degraded-mode signal.
func (f *FailableStore) RecordObjectAndClearPending(ctx context.Context, key, backend string, size int64, enc *core.EncryptionMeta, intentID string) ([]core.DeletedCopy, error) {
	if f.isFailing() {
		return nil, errSimulatedDBOutage
	}
	if f.consumeFailCommitOnce() {
		return nil, errSimulatedCommitFailure
	}
	return f.inner.RecordObjectAndClearPending(ctx, key, backend, size, enc, intentID)
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
func (f *FailableStore) RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, enc *core.EncryptionMeta) error {
	if f.isFailing() {
		return errSimulatedDBOutage
	}
	return f.inner.RecordPart(ctx, uploadID, partNumber, etag, size, enc)
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
func (f *FailableStore) RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error {
	if f.isFailing() {
		return errSimulatedDBOutage
	}
	return f.inner.RemoveExcessCopy(ctx, key, backendName, size)
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
