// -------------------------------------------------------------------------------
// UI Handler - Login Resolution Tests
//
// Author: Alex Freidah
//
// The dashboard logs in against a credential rather than against a password of
// its own, so these cover which identity a submitted keypair proves and which
// submissions prove nothing.
// -------------------------------------------------------------------------------

package ui

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// TestResolveLogin verifies the dashboard logs in against a credential, which
// is what removes admin_key/admin_secret as a mechanism of its own rather than
// a second spelling of the first.
func TestResolveLogin(t *testing.T) {
	t.Parallel()

	view := provisioning.Merge(nil, config.AuthConfig{
		Root: config.RootCredential{AccessKeyID: "AKIAROOT", SecretAccessKey: "root-secret"},
	}, &provisioning.Snapshot{
		Users: []core.User{{ID: "u1", Name: "ops"}},
		Credentials: []core.Credential{
			{AccessKeyID: "AKIALOGIN", UserID: "u1", Secret: "the-secret"},
		},
	})
	registry, err := auth.NewBucketRegistry(&view)
	if err != nil {
		t.Fatalf("NewBucketRegistry: %v", err)
	}
	h := &Handler{registry: func() *auth.BucketRegistry { return registry }}

	for _, tc := range []struct {
		name   string
		key    string
		secret string
		want   string
		ok     bool
	}{
		{"the root credential resolves to root", "AKIAROOT", "root-secret", provisioning.RootUserID, true},
		{"a provisioned credential resolves to its user", "AKIALOGIN", "the-secret", "u1", true},
		{"a wrong secret is refused", "AKIALOGIN", "wrong", "", false},
		{"an unknown key is refused", "AKIANOPE", "the-secret", "", false},
		{"empty is refused", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := h.resolveLogin(tc.key, tc.secret)
			if ok != tc.ok || got != tc.want {
				t.Errorf("resolveLogin(%q) = %q,%v; want %q,%v", tc.key, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestResolveLogin_WithoutARegistry verifies no keypair logs in before a
// registry is published, which is the state a deployment is in while it starts
// up. Refusing is the only safe answer: there is nothing to check against.
func TestResolveLogin_WithoutARegistry(t *testing.T) {
	t.Parallel()

	h := &Handler{}
	if _, ok := h.resolveLogin("AKIAROOT", "root-secret"); ok {
		t.Error("a credential authenticated with no registry to check it against")
	}

	h = &Handler{registry: func() *auth.BucketRegistry { return nil }}
	if _, ok := h.resolveLogin("AKIAROOT", "root-secret"); ok {
		t.Error("a credential authenticated against a nil registry")
	}
}
