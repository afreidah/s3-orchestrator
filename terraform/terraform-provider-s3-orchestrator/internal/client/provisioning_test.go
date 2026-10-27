// -------------------------------------------------------------------------------
// Admin API Client - Provisioning Tests
//
// Author: Alex Freidah
//
// Covers the lookups, which search one listing document rather than fetch one
// resource, and the mutations, which are checked by the method and path they
// put on the wire. A lookup reporting absence rather than an error is the
// behaviour a Read depends on to drop a resource from state.
// -------------------------------------------------------------------------------

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// listing is what the provisioning endpoint answers with throughout. Two users
// so a lookup has something to reject, and a grant of each kind so the one
// carrying no name is covered.
var listing = Provisioning{
	Buckets: []Bucket{{Name: "photos", Source: "config"}},
	Users: []User{
		{
			ID: "user-backup", Name: "temporal-backup-job", Source: "store",
			Grants: []Grant{
				{Kind: "bucket", Name: "photos", Permissions: []string{"list", "read"}},
				{Kind: KindOrchestrator, Permissions: []string{"admin-read"}},
			},
		},
		{ID: "user-aptly", Name: "aptly", Source: SourceConfig},
	},
	Credentials: []Credential{
		{AccessKeyID: "AKIABACKUP0000000000", UserID: "user-backup", Label: "vault"},
		{AccessKeyID: "AKIAAPTLY00000000000", UserID: "user-aptly", Source: SourceConfig},
	},
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// request is what one call put on the wire.
type request struct {
	method string
	path   string
	query  string
	body   string
}

// recorder serves the listing and records whatever else is asked of it, which
// is how a mutation is checked without a real orchestrator behind it.
func recorder(t *testing.T) (*Client, *request) {
	t.Helper()
	var got request
	c := stub(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = request{r.Method, r.URL.Path, r.URL.RawQuery, string(raw)}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(listing)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	return c, &got
}

// -------------------------------------------------------------------------
// LOOKUPS
// -------------------------------------------------------------------------

func TestProvisioningFetchesEverything(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	p, err := c.Provisioning(context.Background())
	if err != nil {
		t.Fatalf("Provisioning: %v", err)
	}
	if got.path != pathProvisioning {
		t.Errorf("path = %q, want %q", got.path, pathProvisioning)
	}
	if len(p.Users) != 2 || len(p.Credentials) != 2 || len(p.Buckets) != 1 {
		t.Errorf("fetched %d users, %d credentials, %d buckets; want 2, 2, 1",
			len(p.Users), len(p.Credentials), len(p.Buckets))
	}
}

// TestLookups covers finding a bucket, a user and a keypair in the listing,
// and that a missing one is absent rather than an error. Each lookup reports
// one field of what it found, so a match on the wrong entry shows.
func TestLookups(t *testing.T) {
	t.Parallel()
	bucket := func(key string) func(*Client) (string, bool, error) {
		return func(c *Client) (string, bool, error) {
			b, found, err := c.Bucket(context.Background(), key)
			return b.Source, found, err
		}
	}
	user := func(key string) func(*Client) (string, bool, error) {
		return func(c *Client) (string, bool, error) {
			u, found, err := c.User(context.Background(), key)
			return u.Name, found, err
		}
	}
	credential := func(key string) func(*Client) (string, bool, error) {
		return func(c *Client) (string, bool, error) {
			cr, found, err := c.Credential(context.Background(), key)
			return cr.UserID, found, err
		}
	}
	cases := []struct {
		name      string
		lookup    func(*Client) (string, bool, error)
		wantFound bool
		want      string
	}{
		{"an existing bucket is found", bucket("photos"), true, "config"},
		{"a missing bucket is absent", bucket("gone"), false, ""},
		{"an existing user is found", user("user-backup"), true, "temporal-backup-job"},
		{"a missing user is absent", user("user-gone"), false, ""},
		{"an existing keypair is found", credential("AKIABACKUP0000000000"), true, "user-backup"},
		{"a revoked keypair is absent", credential("AKIAGONE00000000000"), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := recorder(t)
			got, found, err := tc.lookup(c)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if found != tc.wantFound || got != tc.want {
				t.Errorf("lookup = %q, %v; want %q, %v", got, found, tc.want, tc.wantFound)
			}
		})
	}
}

// TestLookupReportsFailure covers a listing that cannot be fetched surfacing
// as an error rather than as an absent entry.
func TestLookupReportsFailure(t *testing.T) {
	t.Parallel()
	c := stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, _, err := c.User(context.Background(), "user-backup"); err == nil {
		t.Error("User = nil error, want the failure reported")
	}
}

// TestGrantLookup covers the nesting: a grant hangs off its user, so one whose
// user is gone is absent rather than an error, which is the same answer a Read
// wants either way.
func TestGrantLookup(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		userID    string
		kind      string
		resource  string
		wantFound bool
		wantPerms int
	}{
		{"a bucket grant is found", "user-backup", "bucket", "photos", true, 2},
		{"an orchestrator grant carries no name", "user-backup", KindOrchestrator, "", true, 1},
		{"a grant on another bucket is absent", "user-backup", "bucket", "artifacts", false, 0},
		{"a grant whose user is gone is absent", "user-gone", "bucket", "photos", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := recorder(t)

			g, found, err := c.Grant(context.Background(), tc.userID, tc.kind, tc.resource)
			if err != nil {
				t.Fatalf("Grant: %v", err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if len(g.Permissions) != tc.wantPerms {
				t.Errorf("permissions = %v, want %d of them", g.Permissions, tc.wantPerms)
			}
		})
	}
}

// -------------------------------------------------------------------------
// MUTATIONS
// -------------------------------------------------------------------------

func TestCreateBucket(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	_, err := c.CreateBucket(context.Background(), CreateBucketRequest{
		Name: "photos", MaxMultipartUploads: 4,
	})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	assertRequest(t, got, http.MethodPost, pathBuckets,
		`{"name":"photos","max_multipart_uploads":4}`)
}

func TestUpdateBucket(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	req := UpdateBucketRequest{
		CORS: []CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}}},
	}
	if err := c.UpdateBucket(context.Background(), "photos", req); err != nil {
		t.Fatalf("UpdateBucket: %v", err)
	}
	assertRequest(t, got, http.MethodPatch, pathBuckets+"/photos",
		`{"cors":[{"allowed_origins":["*"],"allowed_methods":["GET"]}]}`)
}

// TestUpdateBucketClearsCORS verifies an empty rule set goes out as a body the
// orchestrator reads as "no rules" rather than being dropped, which is what
// makes an update a replacement.
func TestUpdateBucketClearsCORS(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	if err := c.UpdateBucket(context.Background(), "photos", UpdateBucketRequest{}); err != nil {
		t.Fatalf("UpdateBucket: %v", err)
	}
	assertRequest(t, got, http.MethodPatch, pathBuckets+"/photos", `{}`)
}

func TestDeleteBucket(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	if err := c.DeleteBucket(context.Background(), "photos"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	assertRequest(t, got, http.MethodDelete, pathBuckets+"/photos", "")
}

func TestCreateUser(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	out, err := c.CreateUser(context.Background(), "temporal-backup-job")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	assertRequest(t, got, http.MethodPost, pathUsers, `{"name":"temporal-backup-job"}`)
	if out.Status != "ok" {
		t.Errorf("status = %q, want ok", out.Status)
	}
}

func TestRenameUser(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	if err := c.RenameUser(context.Background(), "user-backup", "renamed"); err != nil {
		t.Fatalf("RenameUser: %v", err)
	}
	assertRequest(t, got, http.MethodPatch, pathUsers+"/user-backup", `{"name":"renamed"}`)
}

func TestDeleteUser(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	if err := c.DeleteUser(context.Background(), "user-backup"); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	assertRequest(t, got, http.MethodDelete, pathUsers+"/user-backup", "")
}

func TestCreateCredential(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	_, err := c.CreateCredential(context.Background(), CreateCredentialRequest{
		UserID: "user-backup", Label: "vault",
	})
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	// The keypair fields are omitted rather than sent empty, which is what
	// tells the orchestrator to mint one.
	assertRequest(t, got, http.MethodPost, pathCredentials,
		`{"user_id":"user-backup","label":"vault"}`)
}

func TestDeleteCredential(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	if err := c.DeleteCredential(context.Background(), "AKIABACKUP0000000000"); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	assertRequest(t, got, http.MethodDelete, pathCredentials+"/AKIABACKUP0000000000", "")
}

func TestSetGrant(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	err := c.SetGrant(context.Background(), "user-backup", "bucket", "photos",
		[]string{"list", "read"})
	if err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	assertRequest(t, got, http.MethodPut, pathGrants+"/user-backup/photos",
		`{"permissions":["list","read"]}`)
	if got.query != "kind=bucket" {
		t.Errorf("query = %q, want kind=bucket", got.query)
	}
}

func TestDeleteGrant(t *testing.T) {
	t.Parallel()
	c, got := recorder(t)

	if err := c.DeleteGrant(context.Background(), "user-backup", "bucket", "photos"); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	assertRequest(t, got, http.MethodDelete, pathGrants+"/user-backup/photos", "")
}

// TestGrantPath covers the placeholder segment. A grant on the orchestrator
// carries no name, and a path cannot have an empty segment, so the kind is
// repeated there and the server discards it once the query says what is meant.
func TestGrantPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		userID string
		kind   string
		res    string
		want   string
	}{
		{
			"a named grant uses the resource name", "user-backup", "bucket", "photos",
			pathGrants + "/user-backup/photos?kind=bucket",
		},
		{
			"an unnamed grant repeats the kind as the segment", "user-backup", KindOrchestrator, "",
			pathGrants + "/user-backup/orchestrator?kind=orchestrator",
		},
		{
			"a wildcard survives escaping", "user-backup", "bucket", "*",
			pathGrants + "/user-backup/%2A?kind=bucket",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := grantPath(tc.userID, tc.kind, tc.res); got != tc.want {
				t.Errorf("grantPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// assertRequest checks the method, path and body one call put on the wire.
func assertRequest(t *testing.T, got *request, method, path, body string) {
	t.Helper()
	if got.method != method {
		t.Errorf("method = %q, want %q", got.method, method)
	}
	if got.path != path {
		t.Errorf("path = %q, want %q", got.path, path)
	}
	if got.body != body && got.body != body+"\n" {
		t.Errorf("body = %q, want %q", got.body, body)
	}
}
