// -------------------------------------------------------------------------------
// Admin API - Provisioning Handler Tests
//
// Author: Alex Freidah
//
// What the endpoints render and what status each rejection maps onto. The
// listing is checked for the one thing it must never carry - a secret - and the
// mint for the one place it must.
// -------------------------------------------------------------------------------

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/ops"
	"github.com/afreidah/s3-orchestrator/internal/ops/opstest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// -------------------------------------------------------------------------
// HARNESS
// -------------------------------------------------------------------------

// provRows is what the provisioning store answers its listings with.
type provRows struct {
	buckets     []core.Bucket
	users       []core.User
	credentials []core.Credential
	grants      []core.Grant
}

// provisioningWith installs a provisioning service over a mocked store and a
// config declaring buckets, and returns the store so a test can state the write
// it expects.
func provisioningWith(t *testing.T, h *Handler, cfgBuckets []config.BucketConfig, rows *provRows) *opstest.MockProvisioningStore {
	t.Helper()
	store := opstest.NewMockProvisioningStore(gomock.NewController(t))
	a := gomock.Any()
	store.EXPECT().ListBuckets(a).Return(rows.buckets, nil).AnyTimes()
	store.EXPECT().ListUsers(a).Return(rows.users, nil).AnyTimes()
	store.EXPECT().ListCredentials(a).Return(rows.credentials, nil).AnyTimes()
	store.EXPECT().ListGrants(a).Return(rows.grants, nil).AnyTimes()

	h.provision = ops.NewProvisioning(ops.ProvisioningDeps{
		Store:  store,
		Config: ops.NewConfigStore(&config.Config{Buckets: cfgBuckets}),
	})
	return store
}

// jsonRequest builds a request carrying body as JSON.
func jsonRequest(t *testing.T, method, target string, body any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return httptest.NewRequestWithContext(t.Context(), method, target, bytes.NewReader(raw))
}

// -------------------------------------------------------------------------
// LISTING
// -------------------------------------------------------------------------

// TestHandleProvisioning_ListsBothSources verifies the listing reports what each
// source declares and marks which is which, since that is what tells a caller
// whether an entry can be changed.
func TestHandleProvisioning_ListsBothSources(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	provisioningWith(t, h, []config.BucketConfig{{Name: "from-config"}}, &provRows{
		buckets: []core.Bucket{{Name: "from-store", MaxMultipartUploads: 4}},
		users:   []core.User{{ID: "u1", Name: "ci"}},
		grants:  []core.Grant{{UserID: "u1", BucketName: "from-store"}},
	})

	w := httptest.NewRecorder()
	h.handleProvisioning(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/provisioning", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var got adminapi.ProvisioningResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Buckets) != 2 {
		t.Fatalf("buckets = %+v, want both sources", got.Buckets)
	}
	if got.Buckets[0].Source != adminapi.SourceConfig || got.Buckets[1].Source != adminapi.SourceStore {
		t.Errorf("sources = %q/%q, want config/store", got.Buckets[0].Source, got.Buckets[1].Source)
	}
	if got.Buckets[1].MaxMultipartUploads != 4 {
		t.Errorf("stored bucket limit = %d, want 4", got.Buckets[1].MaxMultipartUploads)
	}
	if len(got.Users) != 1 || len(got.Users[0].Buckets) != 1 {
		t.Errorf("users = %+v, want one reaching one bucket", got.Users)
	}
}

// TestHandleProvisioning_NeverRendersASecret is the property the listing exists
// under: an operator can read what exists without reading what proves it.
func TestHandleProvisioning_NeverRendersASecret(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	provisioningWith(t, h,
		[]config.BucketConfig{
			{Name: "photos", Credentials: []config.CredentialConfig{
				{AccessKeyID: "CFGAK", SecretAccessKey: "config-secret", Token: "config-token"},
			}},
		},
		&provRows{
			users:       []core.User{{ID: "u1", Name: "ci"}},
			credentials: []core.Credential{{AccessKeyID: "STOREAK", UserID: "u1", Secret: "stored-secret"}},
		})

	w := httptest.NewRecorder()
	h.handleProvisioning(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/provisioning", nil))

	body := w.Body.String()
	for _, secret := range []string{"config-secret", "stored-secret", "config-token"} {
		if strings.Contains(body, secret) {
			t.Errorf("the listing rendered %q", secret)
		}
	}
	if !strings.Contains(body, "STOREAK") {
		t.Error("the listing dropped the access key it is supposed to report")
	}
}

// TestHandleProvisioning_ReportsNotices verifies what the merge found reaches
// the operator rather than only the log.
func TestHandleProvisioning_ReportsNotices(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	provisioningWith(t, h, nil, &provRows{
		grants: []core.Grant{{UserID: "ghost", BucketName: "nowhere"}},
	})

	w := httptest.NewRecorder()
	h.handleProvisioning(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/provisioning", nil))

	var got adminapi.ProvisioningResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Notices) != 1 {
		t.Fatalf("notices = %+v, want the dangling grant reported", got.Notices)
	}
}

// TestHandleProvisioning_StoreFailureIs500 verifies a store that cannot be read
// is a fault rather than an empty listing.
func TestHandleProvisioning_StoreFailureIs500(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	store := opstest.NewMockProvisioningStore(gomock.NewController(t))
	store.EXPECT().ListBuckets(gomock.Any()).Return(nil, context.DeadlineExceeded)
	h.provision = ops.NewProvisioning(ops.ProvisioningDeps{
		Store:  store,
		Config: ops.NewConfigStore(&config.Config{}),
	})

	w := httptest.NewRecorder()
	h.handleProvisioning(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/api/provisioning", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// -------------------------------------------------------------------------
// BUCKETS
// -------------------------------------------------------------------------

// TestHandleCreateBucket verifies a declared bucket reaches the store carrying
// what the request stated, CORS rules included.
func TestHandleCreateBucket(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	store := provisioningWith(t, h, nil, &provRows{})

	var stored core.Bucket
	store.EXPECT().CreateBucket(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, b *core.Bucket) error {
			stored = *b
			return nil
		})

	w := httptest.NewRecorder()
	h.handleCreateBucket(w, jsonRequest(t, http.MethodPost, "/admin/api/provisioning/buckets",
		adminapi.CreateBucketRequest{
			Name:                "photos",
			MaxMultipartUploads: 3,
			CORS: []adminapi.CORSRule{{
				AllowedOrigins: []string{"https://example.com"},
				AllowedMethods: []string{"GET"},
				AllowedHeaders: []string{"*"},
				ExposeHeaders:  []string{"ETag"},
				MaxAge:         60,
			}},
		}))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if stored.Name != "photos" || stored.MaxMultipartUploads != 3 {
		t.Errorf("stored bucket = %+v, want the submitted settings", stored)
	}
	if len(stored.CORS) != 1 || stored.CORS[0].MaxAge != 60 {
		t.Errorf("stored CORS = %+v, want the submitted rule", stored.CORS)
	}
}

// TestHandleCreateBucket_MalformedBodyIs400 verifies a body that will not parse
// is the caller's fault rather than a fault.
func TestHandleCreateBucket_MalformedBodyIs400(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	provisioningWith(t, h, nil, &provRows{})

	w := httptest.NewRecorder()
	h.handleCreateBucket(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/admin/api/provisioning/buckets", strings.NewReader("{")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleDeleteBucket verifies removal names the bucket it removed.
func TestHandleDeleteBucket(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	store := provisioningWith(t, h, nil, &provRows{buckets: []core.Bucket{{Name: "photos"}}})
	store.EXPECT().DeleteBucket(gomock.Any(), "photos").Return(nil)

	objects := opstest.NewMockNamespaceCounter(gomock.NewController(t))
	objects.EXPECT().CountObjectsByPrefix(gomock.Any(), "photos/").Return(int64(0), nil)
	h.provision = ops.NewProvisioning(ops.ProvisioningDeps{
		Store:   store,
		Objects: objects,
		Config:  ops.NewConfigStore(&config.Config{}),
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/admin/api/provisioning/buckets/photos", nil)
	req.SetPathValue(paramName, "photos")
	w := httptest.NewRecorder()
	h.handleDeleteBucket(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got adminapi.ProvisioningOperationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Bucket != "photos" {
		t.Errorf("response = %+v, want it to name the bucket", got)
	}
}

// -------------------------------------------------------------------------
// USERS AND CREDENTIALS
// -------------------------------------------------------------------------

// TestHandleCreateUser verifies the generated id comes back, since it is what
// every later call names the user by.
func TestHandleCreateUser(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	store := provisioningWith(t, h, nil, &provRows{})
	store.EXPECT().CreateUser(gomock.Any(), gomock.Any()).Return(nil)

	w := httptest.NewRecorder()
	h.handleCreateUser(w, jsonRequest(t, http.MethodPost, "/admin/api/provisioning/users",
		adminapi.CreateUserRequest{Name: "ci"}))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var got adminapi.ProvisioningOperationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UserID == "" || got.UserName != "ci" {
		t.Errorf("response = %+v, want the generated id and the name", got)
	}
}

// TestHandleDeleteUser verifies removal reaches the store for the id in the
// path.
func TestHandleDeleteUser(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	store := provisioningWith(t, h, nil, &provRows{users: []core.User{{ID: "u1", Name: "ci"}}})
	store.EXPECT().DeleteUser(gomock.Any(), "u1").Return(nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/admin/api/provisioning/users/u1", nil)
	req.SetPathValue(paramID, "u1")
	w := httptest.NewRecorder()
	h.handleDeleteUser(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleCreateCredential_ReturnsTheSecretOnce verifies the mint is the one
// response carrying a secret, and that it carries both halves of the keypair.
func TestHandleCreateCredential_ReturnsTheSecretOnce(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	store := provisioningWith(t, h, nil, &provRows{users: []core.User{{ID: "u1", Name: "ci"}}})
	store.EXPECT().CreateCredential(gomock.Any(), gomock.Any()).Return(nil)

	w := httptest.NewRecorder()
	h.handleCreateCredential(w, jsonRequest(t, http.MethodPost, "/admin/api/provisioning/credentials",
		adminapi.CreateCredentialRequest{UserID: "u1", Label: "deploy job"}))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var got adminapi.CreateCredentialResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AccessKeyID == "" || got.SecretAccessKey == "" {
		t.Fatalf("response = %+v, want both halves of the keypair", got)
	}
	if got.UserID != "u1" || got.Label != "deploy job" {
		t.Errorf("response = %+v, want it to name the user and the label", got)
	}
}

// TestHandleDeleteCredential verifies revocation reaches the store for the
// access key in the path.
func TestHandleDeleteCredential(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	store := provisioningWith(t, h, nil, &provRows{
		users:       []core.User{{ID: "u1", Name: "ci"}},
		credentials: []core.Credential{{AccessKeyID: "AK", UserID: "u1", Secret: "s"}},
	})
	store.EXPECT().DeleteCredential(gomock.Any(), "AK").Return(nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/admin/api/provisioning/credentials/AK", nil)
	req.SetPathValue(paramID, "AK")
	w := httptest.NewRecorder()
	h.handleDeleteCredential(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// -------------------------------------------------------------------------
// GRANTS
// -------------------------------------------------------------------------

// TestHandleCreateGrant verifies the grant reaches the store naming both sides.
func TestHandleCreateGrant(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	store := provisioningWith(t, h, []config.BucketConfig{{Name: "photos"}},
		&provRows{users: []core.User{{ID: "u1", Name: "ci"}}})

	var stored core.Grant
	store.EXPECT().CreateGrant(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, g *core.Grant) error {
			stored = *g
			return nil
		})

	w := httptest.NewRecorder()
	h.handleCreateGrant(w, jsonRequest(t, http.MethodPost, "/admin/api/provisioning/grants",
		adminapi.CreateGrantRequest{UserID: "u1", Bucket: "photos"}))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if stored.UserID != "u1" || stored.BucketName != "photos" {
		t.Errorf("stored grant = %+v, want u1 on photos", stored)
	}
}

// TestHandleDeleteGrant verifies the withdrawal names both sides from the path.
func TestHandleDeleteGrant(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	store := provisioningWith(t, h, []config.BucketConfig{{Name: "photos"}},
		&provRows{
			users:  []core.User{{ID: "u1", Name: "ci"}},
			grants: []core.Grant{{UserID: "u1", BucketName: "photos"}},
		})
	store.EXPECT().DeleteGrant(gomock.Any(), "u1", "photos").Return(nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete,
		"/admin/api/provisioning/grants/u1/photos", nil)
	req.SetPathValue(paramID, "u1")
	req.SetPathValue(paramName, "photos")
	w := httptest.NewRecorder()
	h.handleDeleteGrant(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleCreateGrant_MalformedBodyIs400 verifies an unparseable body is the
// caller's fault.
func TestHandleCreateGrant_MalformedBodyIs400(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	provisioningWith(t, h, nil, &provRows{})

	w := httptest.NewRecorder()
	h.handleCreateGrant(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/admin/api/provisioning/grants", strings.NewReader("{")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleCreateUser_MalformedBodyIs400 verifies the same for the user
// endpoint.
func TestHandleCreateUser_MalformedBodyIs400(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	provisioningWith(t, h, nil, &provRows{})

	w := httptest.NewRecorder()
	h.handleCreateUser(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/admin/api/provisioning/users", strings.NewReader("{")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleCreateCredential_MalformedBodyIs400 verifies the same for the mint
// endpoint.
func TestHandleCreateCredential_MalformedBodyIs400(t *testing.T) {
	t.Parallel()
	h := newCoverageHandler(t)
	provisioningWith(t, h, nil, &provRows{})

	w := httptest.NewRecorder()
	h.handleCreateCredential(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/admin/api/provisioning/credentials", strings.NewReader("{")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// -------------------------------------------------------------------------
// REJECTION STATUSES
// -------------------------------------------------------------------------

// TestProvisioningError_StatusMapping verifies each rejection reaches the caller
// as an answer it can act on rather than as a fault, with its own reason.
func TestProvisioningError_StatusMapping(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"name required", ops.ErrNameRequired, http.StatusBadRequest},
		{"user required", ops.ErrUserRequired, http.StatusBadRequest},
		{"bucket missing", ops.ErrBucketNotFound, http.StatusNotFound},
		{"user missing", ops.ErrUserNotFound, http.StatusNotFound},
		{"credential missing", ops.ErrCredentialNotFound, http.StatusNotFound},
		{"config declared", ops.ErrConfigDeclared, http.StatusForbidden},
		{"bucket exists", ops.ErrBucketExists, http.StatusConflict},
		{"bucket not empty", ops.ErrBucketNotEmpty, http.StatusConflict},
		{"bucket granted", ops.ErrBucketGranted, http.StatusConflict},
		{"user in use", ops.ErrUserInUse, http.StatusConflict},
		{"anything else", context.DeadlineExceeded, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCoverageHandler(t)
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/api/provisioning", nil)
			h.provisioningError(w, r, "failed", tc.err)

			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if tc.want == http.StatusInternalServerError {
				return
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(body["error"], tc.err.Error()) {
				t.Errorf("error = %q, want it to carry the reason", body["error"])
			}
		})
	}
}
