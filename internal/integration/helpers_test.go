// -------------------------------------------------------------------------------
// Integration Test Helpers
//
// Author: Alex Freidah
//
// Shared setup and teardown utilities for integration tests. Uses testcontainers
// to spin up PostgreSQL, MinIO (x3), and Redis containers automatically. No
// external docker-compose required — just `go test -tags integration`.
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

	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

const virtualBucket = "test-bucket"

var (
	proxyAddr         string
	testDB            *sql.DB
	testManager       *proxy.BackendManager
	testStore         *store.Store
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

func TestMain(m *testing.M) {
	// Silence the proxy's request logger so test output is clean.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()

	// ---------------------------------------------------------------
	// Start containers
	// ---------------------------------------------------------------

	pgContainer, err := tcpostgres.Run(ctx,
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

	minioSpecs := []struct {
		name   string
		envKey string
		bucket string
	}{
		{"minio-1", "MINIO1_ENDPOINT", "backend1"},
		{"minio-2", "MINIO2_ENDPOINT", "backend2"},
		{"minio-3", "MINIO3_ENDPOINT", "backend3"},
	}

	minios := make([]minioInstance, len(minioSpecs))
	for i, spec := range minioSpecs {
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
		// Set env vars so envOrDefault() calls elsewhere pick up the right endpoints.
		os.Setenv(spec.envKey, minios[i].endpoint)
	}

	redisContainer, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start redis: %v\n", err)
		os.Exit(1)
	}
	redisConnStr, err := redisContainer.ConnectionString(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get redis endpoint: %v\n", err)
		os.Exit(1)
	}
	// redis module returns "redis://host:port/0" — extract host:port for REDIS_ADDR
	redisAddr := strings.TrimPrefix(redisConnStr, "redis://")
	redisAddr = strings.TrimSuffix(redisAddr, "/0")
	os.Setenv("REDIS_ADDR", redisAddr)

	// ---------------------------------------------------------------
	// Create buckets on each MinIO
	// ---------------------------------------------------------------

	for _, mi := range minios {
		mc := s3.New(s3.Options{
			BaseEndpoint: aws.String(mi.endpoint),
			Region:       "us-east-1",
			Credentials:  credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", ""),
			UsePathStyle: true,
		})
		_, err := mc.CreateBucket(ctx, &s3.CreateBucketInput{
			Bucket: aws.String(mi.bucket),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create bucket %s: %v\n", mi.bucket, err)
			os.Exit(1)
		}
	}

	// ---------------------------------------------------------------
	// Parse Postgres connection details
	// ---------------------------------------------------------------

	pgHost, err := pgContainer.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get postgres host: %v\n", err)
		os.Exit(1)
	}
	pgPort, err := pgContainer.MappedPort(ctx, "5432/tcp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get postgres port: %v\n", err)
		os.Exit(1)
	}

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
			Port:     int(pgPort.Num()),
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

	db, err := store.NewStore(ctx, &cfg.Database)
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
		b, err := s3be.NewS3Backend(&bcfg)
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

	// Wire: store → FailableStore → per-role CB wrappers → manager
	failableStore := newFailableStore(db)
	testFailableStore = failableStore

	dbCB := store.NewDatabaseBreaker(cfg.CircuitBreaker)
	testDatabaseCB = dbCB
	stores := newCBStores(failableStore, dbCB)

	manager := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        testBackends,
		Stores:          stores,
		Dashboard:       store.NewCBDashboardStore(failableStore, dbCB),
		Metrics:         newMetricsAdapter(failableStore, dbCB),
		Order:           testBackendOrder,
		CacheTTL:        60 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
	testManager = manager

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

// resetState truncates all object/multipart tables and resets quota counters.
func resetState(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		"DELETE FROM cleanup_queue",
		"DELETE FROM multipart_parts",
		"DELETE FROM multipart_uploads",
		"DELETE FROM object_locations",
		"UPDATE backend_quotas SET bytes_used = 0, orphan_bytes = 0, updated_at = NOW()",
	} {
		if _, err := testDB.Exec(q); err != nil {
			t.Fatalf("resetState: %v", err)
		}
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
func newThreeBackendManager(t *testing.T) *proxy.BackendManager {
	t.Helper()
	dbCB := store.NewDatabaseBreaker(config.CircuitBreakerConfig{
		FailureThreshold: 3,
		OpenTimeout:      500 * time.Millisecond,
		CacheTTL:         60 * time.Second,
	})
	return proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        allBackends,
		Stores:          newCBStores(testFailableStore, dbCB),
		Dashboard:       store.NewCBDashboardStore(testFailableStore, dbCB),
		Metrics:         newMetricsAdapter(testFailableStore, dbCB),
		Order:           allBackendOrder,
		CacheTTL:        60 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: config.RoutingPack,
	})
}

// newCBStores wraps a role-bearing value (FailableStore or *store.Store)
// with per-role CB decorators sharing a single *breaker.CircuitBreaker.
func newCBStores(src roleStore, cb *breaker.CircuitBreaker) proxy.Stores {
	return proxy.Stores{
		Object:           store.NewCBObjectStore(src, cb),
		Quota:            store.NewCBQuotaStore(src, cb),
		Multipart:        store.NewCBMultipartStore(src, cb),
		Replication:      store.NewCBReplicationStore(src, cb),
		Cleanup:          store.NewCBCleanupStore(src, cb),
		Integrity:        store.NewCBIntegrityStore(src, cb),
		Lifecycle:        store.NewCBLifecycleStore(src, cb),
		BackendLifecycle: store.NewCBBackendLifecycleStore(src, cb),
		Dashboard:        store.NewCBDashboardStore(src, cb),
		Usage:            store.NewCBUsageFlusher(src, cb),
		Lock:             store.NewAdvisoryLocker(src),
	}
}

// roleStore names the role union tests need on a single source value.
type roleStore interface {
	store.ObjectStore
	store.QuotaStore
	store.MultipartStore
	store.ReplicationStore
	store.CleanupStore
	store.IntegrityStore
	store.LifecycleStore
	store.BackendLifecycleStore
	store.DashboardStore
	store.UsageFlusher
	store.AdvisoryLocker
}

// metricsAdapter carries the CB-wrapped role views MetricsCollector needs.
type metricsAdapter struct {
	store.DashboardStore
	store.ReplicationStore
}

// newMetricsAdapter returns a proxy.MetricsDeps-compatible value.
func newMetricsAdapter(src roleStore, cb *breaker.CircuitBreaker) *metricsAdapter {
	return &metricsAdapter{
		DashboardStore:   store.NewCBDashboardStore(src, cb),
		ReplicationStore: store.NewCBReplicationStore(src, cb),
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// -------------------------------------------------------------------------
// FailableStore — injectable failure wrapper for circuit breaker tests
// -------------------------------------------------------------------------

// errSimulatedDBFailure simulates a database connection error.
var errSimulatedDBFailure = errors.New("simulated database connection failure")

// FailableStore wraps a concrete metadata store and can be toggled to return
// connection errors, simulating a database outage for circuit breaker
// integration tests. It embeds every narrow role interface so one instance
// satisfies any role the proxy asks for; a single *store.Store is assigned
// to every embedded field.
type FailableStore struct {
	store.ObjectStore
	store.QuotaStore
	store.MultipartStore
	store.ReplicationStore
	store.CleanupStore
	store.IntegrityStore
	store.LifecycleStore
	store.BackendLifecycleStore
	store.DashboardStore
	store.UsageFlusher
	store.AdvisoryLocker
	inner   *store.Store
	mu      sync.Mutex
	failing bool
}

// newFailableStore returns a FailableStore whose role views all resolve to
// the same concrete *store.Store.
func newFailableStore(db *store.Store) *FailableStore {
	return &FailableStore{
		ObjectStore:           db,
		QuotaStore:            db,
		MultipartStore:        db,
		ReplicationStore:      db,
		CleanupStore:          db,
		IntegrityStore:        db,
		LifecycleStore:        db,
		BackendLifecycleStore: db,
		DashboardStore:        db,
		UsageFlusher:          db,
		AdvisoryLocker:        db,
		inner:                 db,
	}
}

func (f *FailableStore) SetFailing(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = v
}

func (f *FailableStore) isFailing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failing
}

func (f *FailableStore) GetAllObjectLocations(ctx context.Context, key string) ([]store.ObjectLocation, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.GetAllObjectLocations(ctx, key)
}

func (f *FailableStore) RecordObject(ctx context.Context, key, backend string, size int64, enc *store.EncryptionMeta) ([]store.DeletedCopy, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.RecordObject(ctx, key, backend, size, enc)
}

func (f *FailableStore) DeleteObject(ctx context.Context, key string) ([]store.DeletedCopy, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.DeleteObject(ctx, key)
}

func (f *FailableStore) ListObjects(ctx context.Context, prefix, startAfter string, maxKeys int) (*store.ListObjectsResult, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.ListObjects(ctx, prefix, startAfter, maxKeys)
}

func (f *FailableStore) GetBackendWithSpace(ctx context.Context, size int64, backendOrder []string) (string, error) {
	if f.isFailing() {
		return "", errSimulatedDBFailure
	}
	return f.inner.GetBackendWithSpace(ctx, size, backendOrder)
}

func (f *FailableStore) GetLeastUtilizedBackend(ctx context.Context, size int64, eligible []string) (string, error) {
	if f.isFailing() {
		return "", errSimulatedDBFailure
	}
	return f.inner.GetLeastUtilizedBackend(ctx, size, eligible)
}

func (f *FailableStore) CreateMultipartUpload(ctx context.Context, uploadID, key, backend, contentType string, metadata map[string]string) error {
	if f.isFailing() {
		return errSimulatedDBFailure
	}
	return f.inner.CreateMultipartUpload(ctx, uploadID, key, backend, contentType, metadata)
}

func (f *FailableStore) GetMultipartUpload(ctx context.Context, uploadID string) (*store.MultipartUpload, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.GetMultipartUpload(ctx, uploadID)
}

func (f *FailableStore) RecordPart(ctx context.Context, uploadID string, partNumber int, etag string, size int64, enc *store.EncryptionMeta) error {
	if f.isFailing() {
		return errSimulatedDBFailure
	}
	return f.inner.RecordPart(ctx, uploadID, partNumber, etag, size, enc)
}

func (f *FailableStore) GetParts(ctx context.Context, uploadID string) ([]store.MultipartPart, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.GetParts(ctx, uploadID)
}

func (f *FailableStore) DeleteMultipartUpload(ctx context.Context, uploadID string) error {
	if f.isFailing() {
		return errSimulatedDBFailure
	}
	return f.inner.DeleteMultipartUpload(ctx, uploadID)
}

func (f *FailableStore) GetQuotaStats(ctx context.Context) (map[string]store.QuotaStat, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.GetQuotaStats(ctx)
}

func (f *FailableStore) GetObjectCounts(ctx context.Context) (map[string]int64, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.GetObjectCounts(ctx)
}

func (f *FailableStore) GetActiveMultipartCounts(ctx context.Context) (map[string]int64, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.GetActiveMultipartCounts(ctx)
}

func (f *FailableStore) GetStaleMultipartUploads(ctx context.Context, olderThan time.Duration) ([]store.MultipartUpload, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.GetStaleMultipartUploads(ctx, olderThan)
}

func (f *FailableStore) ListDirectoryChildren(ctx context.Context, prefix, startAfter string, maxKeys int) (*store.DirectoryListResult, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.ListDirectoryChildren(ctx, prefix, startAfter, maxKeys)
}

func (f *FailableStore) ListObjectsByBackend(ctx context.Context, backendName string, limit int) ([]store.ObjectLocation, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.ListObjectsByBackend(ctx, backendName, limit)
}

func (f *FailableStore) MoveObjectLocation(ctx context.Context, key, fromBackend, toBackend string) (int64, error) {
	if f.isFailing() {
		return 0, errSimulatedDBFailure
	}
	return f.inner.MoveObjectLocation(ctx, key, fromBackend, toBackend)
}

func (f *FailableStore) GetUnderReplicatedObjects(ctx context.Context, factor, limit int) ([]store.ObjectLocation, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.GetUnderReplicatedObjects(ctx, factor, limit)
}

func (f *FailableStore) RecordReplica(ctx context.Context, key, targetBackend, sourceBackend string, size int64) (bool, error) {
	if f.isFailing() {
		return false, errSimulatedDBFailure
	}
	return f.inner.RecordReplica(ctx, key, targetBackend, sourceBackend, size)
}

func (f *FailableStore) GetOverReplicatedObjects(ctx context.Context, factor, limit int) ([]store.ObjectLocation, error) {
	if f.isFailing() {
		return nil, errSimulatedDBFailure
	}
	return f.inner.GetOverReplicatedObjects(ctx, factor, limit)
}

func (f *FailableStore) CountOverReplicatedObjects(ctx context.Context, factor int) (int64, error) {
	if f.isFailing() {
		return 0, errSimulatedDBFailure
	}
	return f.inner.CountOverReplicatedObjects(ctx, factor)
}

func (f *FailableStore) RemoveExcessCopy(ctx context.Context, key, backendName string, size int64) error {
	if f.isFailing() {
		return errSimulatedDBFailure
	}
	return f.inner.RemoveExcessCopy(ctx, key, backendName, size)
}

// tripCircuitBreaker makes enough failing requests to trip the circuit breaker open.
func tripCircuitBreaker(t *testing.T) {
	t.Helper()
	client := newS3Client(t)
	ctx := context.Background()
	// The default failure threshold is 3 — make enough failing requests
	for i := 0; i < 5; i++ {
		client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(fmt.Sprintf("trip-circuit-%d", i)),
		})
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

	backend, err := s3be.NewS3Backend(&cfg)
	if err != nil {
		t.Fatalf("NewS3Backend(%s): %v", name, err)
	}
	return backend
}
