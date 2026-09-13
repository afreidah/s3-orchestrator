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

	"github.com/afreidah/s3-orchestrator/internal/provisioning"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// TestResolveLogin verifies the dashboard logs in against a credential, which
// is what removes admin_key/admin_secret as a mechanism of its own rather than
// a second spelling of the first.
func TestResolveLogin(t *testing.T) {
	t.Parallel()

	view := provisioning.View{
		Users: []provisioning.User{{ID: "u1", Name: "ops", Source: provisioning.SourceStore}},
		Credentials: []provisioning.Credential{{
			AccessKeyID: "AKIALOGIN",
			UserID:      "u1",
			Secret:      "the-secret",
			Source:      provisioning.SourceStore,
		}},
	}
	registry, err := auth.NewBucketRegistry(&view)
	if err != nil {
		t.Fatalf("NewBucketRegistry: %v", err)
	}
	h := &Handler{
		adminKey:    "admin",
		adminSecret: "admin-pass",
		registry:    func() *auth.BucketRegistry { return registry },
	}

	for _, tc := range []struct {
		name   string
		key    string
		secret string
		want   string
		ok     bool
	}{
		{"the configured dashboard login resolves to root", "admin", "admin-pass", provisioning.RootUserID, true},
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

// TestResolveLogin_WithoutARegistry verifies the dashboard still logs in with
// the configured credential before any registry is published, which is the
// state a deployment is in while it starts up.
func TestResolveLogin_WithoutARegistry(t *testing.T) {
	t.Parallel()

	h := &Handler{adminKey: "admin", adminSecret: "admin-pass"}
	if got, ok := h.resolveLogin("admin", "admin-pass"); !ok || got != provisioning.RootUserID {
		t.Errorf("resolveLogin = %q,%v; want the root user", got, ok)
	}
	if _, ok := h.resolveLogin("AKIALOGIN", "the-secret"); ok {
		t.Error("a credential authenticated with no registry to check it against")
	}
}
