// -------------------------------------------------------------------------------
// Provisioning - Integration Tests
//
// Author: Alex Freidah
//
// The chain a mock cannot stand in for: a credential minted over the admin API
// lands in Postgres, the registry is rebuilt, and a real SigV4 request signed
// with it is served. Revoking it refuses the same request on the next call, with
// no reload and no restart.
//
// The refusals are here for the same reason. A bucket that still holds objects
// and a user that still holds a credential are only meaningful against real rows
// and a real object written through the data path, which is also what proves the
// bucket-name-to-key-prefix mapping the count relies on.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/di"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/proxy/proxytest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
	"github.com/afreidah/s3-orchestrator/internal/transport/s3api"
)

// -------------------------------------------------------------------------
// HARNESS
// -------------------------------------------------------------------------

// provEnv is a running instance whose registry is republished by the same
// publisher production uses, so a provisioning change reaches the request path
// exactly as it would in a deployment.
type provEnv struct {
	proxyAddr string
	adminAddr string
	declared  *provisioning.Declared
	cfg       *config.Config
}

// setupProvEnv stands up a proxy and an admin server over the shared Postgres
// store, wired through di.NewRegistryPublisher so a mutation rebuilds and swaps
// the live registry.
func setupProvEnv(t *testing.T) *provEnv {
	t.Helper()
	resetState(t)
	resetProvisioning(t)

	stores := newStores(testStore)
	st := proxytest.New(t, stores, &proxytest.StackOptions{
		Runtime: proxytest.NewRuntime(&proxytest.RuntimeOptions{
			Backends:        testBackends,
			Order:           testBackendOrder,
			RoutingStrategy: config.RoutingPack,
			Metrics:         newMetricsAdapter(testStore),
		}),
	})
	registerStack(t, st)
	workers := proxytest.BuildWorkers(st, stores)

	cfg := &config.Config{Buckets: []config.BucketConfig{{
		Name:        virtualBucket,
		Credentials: []config.CredentialConfig{{AccessKeyID: "test", SecretAccessKey: "test"}},
	}}}

	srv := &s3api.Server{Objects: st.Objects, Multipart: st.Multipart}
	declared := provisioning.NewDeclared()

	inj := do.New()
	do.ProvideValue(inj, cfg)
	do.ProvideValue[core.ProvisioningStore](inj, testStore)
	do.ProvideValue(inj, declared)
	do.ProvideValue(inj, srv)

	publisher := di.NewRegistryPublisher(inj)
	if err := publisher.Republish(context.Background()); err != nil {
		t.Fatalf("initial republish: %v", err)
	}

	opsSvc := ops.New(&ops.Deps{
		Objects:      st.Objects,
		Store:        testStore,
		EncStore:     testStore,
		CompStore:    testStore,
		Runtime:      st.Runtime,
		Usage:        st.Runtime.Usage(),
		IntegrityCfg: st.IntegrityCfg,
		Replicator:   workers.Replicator,
		OverRep:      workers.OverReplicationCleaner,
		Rebalancer:   workers.Rebalancer,
		Scrubber:     workers.Scrubber,
		Provisioning: testStore,
		Registry:     publisher,
		Declared:     declared,
		Cfg:          cfg,
	})

	env := &provEnv{declared: declared, cfg: cfg}
	env.proxyAddr = serveHTTP(t, srv)
	env.adminAddr = serveHTTP(t, provAdminMux(t, opsSvc, st))
	return env
}

// provAdminMux builds the admin handler's mux with the provisioning service
// wired, which is the dependency the hand-built harness has to supply.
func provAdminMux(t *testing.T, opsSvc *ops.Services, st *proxytest.Stack) http.Handler {
	t.Helper()
	var lv slog.LevelVar
	lv.Set(slog.LevelInfo)
	h := admin.New(&admin.Deps{
		BackendOps:  st.Usage,
		Objects:     opsSvc.Objects,
		Integrity:   opsSvc.Integrity,
		Replication: opsSvc.Replication,
		Rebalance:   opsSvc.Rebalance,
		Encryption:  opsSvc.Encryption,
		Compression: opsSvc.Compression,
		Provision:   opsSvc.Provision,
		Drain:       st.Drain,
		Lifecycle:   testStore,
		DBHealthy:   testDatabaseCB.IsHealthy,
		Cleanup:     testStore,
		Token:       adminToken,
		Registry:    func() *auth.BucketRegistry { return nil },
		LogLevel:    &lv,
	})
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

// serveHTTP starts a handler on an ephemeral port and stops it with the test.
func serveHTTP(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return ln.Addr().String()
}

// resetProvisioning empties the provisioning tables in dependency order, so a
// test starts against a store declaring nothing.
func resetProvisioning(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		"DELETE FROM grants",
		"DELETE FROM credentials",
		"DELETE FROM users",
		"DELETE FROM buckets",
	} {
		if _, err := testDB.Exec(q); err != nil {
			t.Fatalf("resetProvisioning: %v", err)
		}
	}
}

// -------------------------------------------------------------------------
// ADMIN CALLS
// -------------------------------------------------------------------------

// admReq issues one admin request and decodes the body into out when non-nil.
// Returns the status so a test can assert a refusal.
func (env *provEnv) admReq(t *testing.T, method, path string, body, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, "http://"+env.adminAddr+path, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Admin-Token", adminToken)

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatalf("admin %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if out != nil && resp.StatusCode < 400 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s: %v (body=%s)", path, err, raw)
		}
	}
	return resp.StatusCode
}

// mustAdmin issues an admin call that has to succeed.
func (env *provEnv) mustAdmin(t *testing.T, method, path string, body, out any) {
	t.Helper()
	if code := env.admReq(t, method, path, body, out); code >= 400 {
		t.Fatalf("admin %s %s: status %d", method, path, code)
	}
}

// onboard creates a bucket, a user and a keypair, grants the user the bucket,
// and returns the minted credential and the user id.
func (env *provEnv) onboard(t *testing.T, bucket, name string) (adminapi.CreateCredentialResponse, string) {
	t.Helper()
	env.mustAdmin(t, http.MethodPost, "/admin/api/provisioning/buckets",
		adminapi.CreateBucketRequest{Name: bucket}, nil)

	var user adminapi.ProvisioningOperationResponse
	env.mustAdmin(t, http.MethodPost, "/admin/api/provisioning/users",
		adminapi.CreateUserRequest{Name: name}, &user)

	var cred adminapi.CreateCredentialResponse
	env.mustAdmin(t, http.MethodPost, "/admin/api/provisioning/credentials",
		adminapi.CreateCredentialRequest{UserID: user.UserID, Label: name}, &cred)

	env.mustAdmin(t, http.MethodPost, "/admin/api/provisioning/grants",
		adminapi.CreateGrantRequest{UserID: user.UserID, Bucket: bucket}, nil)

	return cred, user.UserID
}

// clientFor builds an S3 client signing with a minted keypair.
func (env *provEnv) clientFor(c adminapi.CreateCredentialResponse) *s3.Client {
	return s3.New(s3.Options{
		BaseEndpoint: aws.String("http://" + env.proxyAddr),
		Region:       "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(
			c.AccessKeyID, c.SecretAccessKey, ""),
		UsePathStyle: true,
	})
}

// putObject writes one object and reports the error verbatim.
func putObject(ctx context.Context, c *s3.Client, bucket, key, body string) error {
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(body),
	})
	return err
}

// -------------------------------------------------------------------------
// THE AUTHENTICATION CHAIN
// -------------------------------------------------------------------------

// TestProvInt_MintedCredentialAuthenticates is the property the whole feature
// rests on: a keypair created over the admin API signs a real SigV4 request on
// the next call, with no reload and no restart.
func TestProvInt_MintedCredentialAuthenticates(t *testing.T) {
	env := setupProvEnv(t)
	ctx := context.Background()
	bucket := "minted-bucket"

	cred, _ := env.onboard(t, bucket, "nightly-backup")
	client := env.clientFor(cred)

	if err := putObject(ctx, client, bucket, "hello.txt", "payload"); err != nil {
		t.Fatalf("PUT with the minted credential: %v", err)
	}

	got, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("hello.txt"),
	})
	if err != nil {
		t.Fatalf("GET with the minted credential: %v", err)
	}
	defer got.Body.Close()
	body, _ := io.ReadAll(got.Body)
	if string(body) != "payload" {
		t.Errorf("body = %q, want payload", body)
	}
}

// TestProvInt_RevocationIsImmediate verifies a revoked keypair stops
// authenticating on the next request. Asserted against a running server rather
// than a mock, because what makes it immediate is the registry swap the write
// performs before it returns.
func TestProvInt_RevocationIsImmediate(t *testing.T) {
	env := setupProvEnv(t)
	ctx := context.Background()
	bucket := "revoke-bucket"

	cred, _ := env.onboard(t, bucket, "short-lived")
	client := env.clientFor(cred)

	if err := putObject(ctx, client, bucket, "before.txt", "ok"); err != nil {
		t.Fatalf("PUT before revocation: %v", err)
	}

	env.mustAdmin(t, http.MethodDelete,
		"/admin/api/provisioning/credentials/"+cred.AccessKeyID, nil, nil)

	if err := putObject(ctx, client, bucket, "after.txt", "denied"); err == nil {
		t.Fatal("the revoked credential still authenticates")
	}
}

// TestProvInt_GrantIsWhatAuthorizes verifies a credential with no grant reaches
// nothing, and that adding the grant opens exactly that bucket. This is the
// user-to-grant-to-bucket chain resolving against real rows.
func TestProvInt_GrantIsWhatAuthorizes(t *testing.T) {
	env := setupProvEnv(t)
	ctx := context.Background()
	bucket := "ungranted-bucket"

	env.mustAdmin(t, http.MethodPost, "/admin/api/provisioning/buckets",
		adminapi.CreateBucketRequest{Name: bucket}, nil)

	var user adminapi.ProvisioningOperationResponse
	env.mustAdmin(t, http.MethodPost, "/admin/api/provisioning/users",
		adminapi.CreateUserRequest{Name: "ungranted"}, &user)

	var cred adminapi.CreateCredentialResponse
	env.mustAdmin(t, http.MethodPost, "/admin/api/provisioning/credentials",
		adminapi.CreateCredentialRequest{UserID: user.UserID}, &cred)

	client := env.clientFor(cred)
	if err := putObject(ctx, client, bucket, "no.txt", "denied"); err == nil {
		t.Fatal("a credential holding no grant reached a bucket")
	}

	env.mustAdmin(t, http.MethodPost, "/admin/api/provisioning/grants",
		adminapi.CreateGrantRequest{UserID: user.UserID, Bucket: bucket}, nil)

	if err := putObject(ctx, client, bucket, "yes.txt", "ok"); err != nil {
		t.Fatalf("PUT after the grant: %v", err)
	}
}

// TestProvInt_SiblingCredentialSurvivesRevocation verifies revoking one keypair
// leaves the others working, which is what makes rotation possible without a
// window where the client cannot write.
func TestProvInt_SiblingCredentialSurvivesRevocation(t *testing.T) {
	env := setupProvEnv(t)
	ctx := context.Background()
	bucket := "rotate-bucket"

	first, userID := env.onboard(t, bucket, "rotating")

	var second adminapi.CreateCredentialResponse
	env.mustAdmin(t, http.MethodPost, "/admin/api/provisioning/credentials",
		adminapi.CreateCredentialRequest{UserID: userID, Label: "replacement"}, &second)

	env.mustAdmin(t, http.MethodDelete,
		"/admin/api/provisioning/credentials/"+first.AccessKeyID, nil, nil)

	if err := putObject(ctx, env.clientFor(first), bucket, "old.txt", "denied"); err == nil {
		t.Error("the revoked keypair still authenticates")
	}
	if err := putObject(ctx, env.clientFor(second), bucket, "new.txt", "ok"); err != nil {
		t.Errorf("revoking one keypair broke its sibling: %v", err)
	}
}

// -------------------------------------------------------------------------
// THE DECLARED SET REACHES EVERY SUBSYSTEM
// -------------------------------------------------------------------------

// TestProvInt_StoredBucketReachesAdminEndpoints verifies a bucket created over
// the API is addressable by the admin object endpoints, not only by the S3 data
// path. Reading the config file alone would let an operator write an object and
// then be told by every admin endpoint that its key names no bucket.
func TestProvInt_StoredBucketReachesAdminEndpoints(t *testing.T) {
	env := setupProvEnv(t)
	ctx := context.Background()
	bucket := "admin-visible"

	cred, _ := env.onboard(t, bucket, "writer")
	if err := putObject(ctx, env.clientFor(cred), bucket, "seen.txt", "payload"); err != nil {
		t.Fatalf("PUT: %v", err)
	}

	var locations adminapi.ObjectLocationsResponse
	env.mustAdmin(t, http.MethodGet,
		"/admin/api/object-locations?key="+bucket+"/seen.txt", nil, &locations)
	if len(locations.Locations) == 0 {
		t.Error("the admin endpoint reports no copies of an object it can see over S3")
	}
}

// TestProvInt_StoredBucketCarriesItsCORSRules verifies rules submitted with a
// stored bucket survive into the declared set the browser policy is compiled
// from, rather than being accepted and silently dropped.
func TestProvInt_StoredBucketCarriesItsCORSRules(t *testing.T) {
	env := setupProvEnv(t)

	env.mustAdmin(t, http.MethodPost, "/admin/api/provisioning/buckets",
		adminapi.CreateBucketRequest{
			Name: "cors-bucket",
			CORS: []adminapi.CORSRule{{
				AllowedOrigins: []string{"https://app.example.com"},
				AllowedMethods: []string{"GET"},
			}},
		}, nil)

	for _, b := range env.declared.Buckets() {
		if b.Name != "cors-bucket" {
			continue
		}
		if len(b.CORS) != 1 || b.CORS[0].AllowedOrigins[0] != "https://app.example.com" {
			t.Fatalf("declared bucket CORS = %+v, want the submitted rule", b.CORS)
		}
		return
	}
	t.Fatal("the stored bucket never reached the declared set")
}

// TestProvInt_RejectsUnreadableCORSRule verifies a rule the matcher cannot read
// is refused at creation. Storing it would compile cleanly here and then fail
// every later registry rebuild, taking the fleet's reloads down.
func TestProvInt_RejectsUnreadableCORSRule(t *testing.T) {
	env := setupProvEnv(t)

	code := env.admReq(t, http.MethodPost, "/admin/api/provisioning/buckets",
		adminapi.CreateBucketRequest{
			Name: "bad-cors",
			CORS: []adminapi.CORSRule{{
				AllowedOrigins: []string{"https://*.*.example.com"},
				AllowedMethods: []string{"GET"},
			}},
		}, nil)
	if code < 400 {
		t.Fatalf("status = %d, want a refusal", code)
	}
}

// -------------------------------------------------------------------------
// REFUSALS AGAINST REAL DATA
// -------------------------------------------------------------------------

// TestProvInt_BucketDeleteRefusedWhileObjectsExist verifies the count runs
// against real rows written through the data path, which is also what proves
// the bucket-name-to-key-prefix mapping behind it.
func TestProvInt_BucketDeleteRefusedWhileObjectsExist(t *testing.T) {
	env := setupProvEnv(t)
	ctx := context.Background()
	bucket := "occupied"

	cred, userID := env.onboard(t, bucket, "occupant")
	if err := putObject(ctx, env.clientFor(cred), bucket, "kept.txt", "bytes"); err != nil {
		t.Fatalf("PUT: %v", err)
	}

	// The grant is withdrawn first so the refusal under test is the object
	// count rather than the grant check that precedes nothing.
	env.mustAdmin(t, http.MethodDelete,
		fmt.Sprintf("/admin/api/provisioning/grants/%s/%s", userID, bucket), nil, nil)

	code := env.admReq(t, http.MethodDelete, "/admin/api/provisioning/buckets/"+bucket, nil, nil)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while the bucket holds objects", code)
	}
}

// TestProvInt_BucketDeleteRefusedWhileGranted verifies a bucket a user still
// reaches stays declared, so the grant does not become a dangling one reported
// on every assembly.
func TestProvInt_BucketDeleteRefusedWhileGranted(t *testing.T) {
	env := setupProvEnv(t)
	bucket := "granted"

	env.onboard(t, bucket, "holder")

	code := env.admReq(t, http.MethodDelete, "/admin/api/provisioning/buckets/"+bucket, nil, nil)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while the bucket is granted", code)
	}
}

// TestProvInt_EmptyBucketDeletes verifies the happy path: once nothing depends
// on it, the bucket is removed and stops being reachable.
func TestProvInt_EmptyBucketDeletes(t *testing.T) {
	env := setupProvEnv(t)
	ctx := context.Background()
	bucket := "transient"

	cred, userID := env.onboard(t, bucket, "brief")
	client := env.clientFor(cred)
	if err := putObject(ctx, client, bucket, "gone.txt", "bytes"); err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("gone.txt"),
	}); err != nil {
		t.Fatalf("DELETE object: %v", err)
	}
	env.mustAdmin(t, http.MethodDelete,
		fmt.Sprintf("/admin/api/provisioning/grants/%s/%s", userID, bucket), nil, nil)

	env.mustAdmin(t, http.MethodDelete, "/admin/api/provisioning/buckets/"+bucket, nil, nil)

	if env.declared.Contains(bucket) {
		t.Error("the deleted bucket is still declared")
	}
	if err := putObject(ctx, client, bucket, "after.txt", "denied"); err == nil {
		t.Error("the deleted bucket still accepts writes")
	}
}

// TestProvInt_UserDeleteRefusedWhileHolding verifies the schema's foreign keys
// and the operation's own check agree: a user holding a credential or a grant
// is refused with the reason rather than a constraint violation.
func TestProvInt_UserDeleteRefusedWhileHolding(t *testing.T) {
	env := setupProvEnv(t)
	bucket := "held"

	cred, userID := env.onboard(t, bucket, "holder")

	if code := env.admReq(t, http.MethodDelete,
		"/admin/api/provisioning/users/"+userID, nil, nil); code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while the user holds a grant", code)
	}

	env.mustAdmin(t, http.MethodDelete,
		fmt.Sprintf("/admin/api/provisioning/grants/%s/%s", userID, bucket), nil, nil)

	if code := env.admReq(t, http.MethodDelete,
		"/admin/api/provisioning/users/"+userID, nil, nil); code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while the user holds a credential", code)
	}

	env.mustAdmin(t, http.MethodDelete,
		"/admin/api/provisioning/credentials/"+cred.AccessKeyID, nil, nil)
	env.mustAdmin(t, http.MethodDelete, "/admin/api/provisioning/users/"+userID, nil, nil)
}

// -------------------------------------------------------------------------
// CONFIG PRECEDENCE
// -------------------------------------------------------------------------

// TestProvInt_ConfigDeclaredIsReadOnly verifies the API refuses to remove what
// the config file declares, so an operator reading that file can trust it.
func TestProvInt_ConfigDeclaredIsReadOnly(t *testing.T) {
	env := setupProvEnv(t)

	code := env.admReq(t, http.MethodDelete,
		"/admin/api/provisioning/buckets/"+virtualBucket, nil, nil)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a config-declared bucket", code)
	}
}

// TestProvInt_ConfigWinsAName verifies a stored bucket sharing a config bucket's
// name is dropped from the merge and reported, rather than shadowing the file.
func TestProvInt_ConfigWinsAName(t *testing.T) {
	env := setupProvEnv(t)

	// The name is inserted straight into the store, since the API refuses to
	// create one config already declares.
	if _, err := testDB.Exec(
		"INSERT INTO buckets (name, max_multipart_uploads) VALUES ($1, $2)",
		virtualBucket, 99); err != nil {
		t.Fatalf("insert shadowing bucket: %v", err)
	}

	var view adminapi.ProvisioningResponse
	env.mustAdmin(t, http.MethodGet, "/admin/api/provisioning", nil, &view)

	seen := 0
	for _, b := range view.Buckets {
		if b.Name != virtualBucket {
			continue
		}
		seen++
		if b.Source != adminapi.SourceConfig {
			t.Errorf("source = %q, want config to win the name", b.Source)
		}
		if b.MaxMultipartUploads == 99 {
			t.Error("the stored bucket's settings overrode the config file's")
		}
	}
	if seen != 1 {
		t.Fatalf("the name appears %d times, want the merge to keep one", seen)
	}
	if len(view.Notices) == 0 {
		t.Error("the shadowed bucket was dropped without reporting it")
	}
}

// TestProvInt_ListingNeverRendersASecret verifies the secret exists on the wire
// exactly once. A listing that leaked it would hand every admin-token holder
// every client's signing key.
func TestProvInt_ListingNeverRendersASecret(t *testing.T) {
	env := setupProvEnv(t)

	cred, _ := env.onboard(t, "secret-check", "holder")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+env.adminAddr+"/admin/api/provisioning", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Admin-Token", adminToken)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: test server URL
	if err != nil {
		t.Fatalf("GET provisioning: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if strings.Contains(string(body), cred.SecretAccessKey) {
		t.Error("the listing rendered a credential secret")
	}
	if !strings.Contains(string(body), cred.AccessKeyID) {
		t.Error("the listing dropped the access key it is supposed to report")
	}
}

// -------------------------------------------------------------------------
// REGISTRY REBUILD
// -------------------------------------------------------------------------

// TestProvInt_RebuildPreservesStoredState verifies reassembling the registry
// from config merged with the store keeps everything the API created. Rebuilding
// from the file alone is what a reload would do wrong, silently deleting every
// bucket and credential anyone provisioned.
func TestProvInt_RebuildPreservesStoredState(t *testing.T) {
	env := setupProvEnv(t)
	ctx := context.Background()
	bucket := "survives-reload"

	cred, _ := env.onboard(t, bucket, "persistent")

	view, err := provisioning.LoadMerged(ctx, testStore, env.cfg.Buckets)
	if err != nil {
		t.Fatalf("LoadMerged: %v", err)
	}
	registry, err := auth.NewBucketRegistry(&view)
	if err != nil {
		t.Fatalf("NewBucketRegistry: %v", err)
	}

	if !env.declared.Contains(bucket) {
		t.Error("the stored bucket is missing from the declared set")
	}
	if registry.MaxMultipartUploads(virtualBucket) != 0 {
		t.Error("the config bucket lost its settings across the rebuild")
	}

	if err := putObject(ctx, env.clientFor(cred), bucket, "still.txt", "ok"); err != nil {
		t.Errorf("the credential stopped working across a rebuild: %v", err)
	}
}
