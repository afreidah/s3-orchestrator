// -------------------------------------------------------------------------------
// Auth - Object Key Authorization Tests
//
// Author: Alex Freidah
//
// Covers each answer AuthorizeKey gives: a named bucket granted and not, a key
// naming no single bucket with and without the wildcard, and a nil user.
// -------------------------------------------------------------------------------

package auth

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// TestAuthorizeKey verifies the bucket a key names decides the answer, and that
// a key naming no single bucket needs the bucket wildcard.
func TestAuthorizeKey(t *testing.T) {
	t.Parallel()
	reader := NewUser("r", "reader", map[string]core.PermissionSet{"photos": core.PermRead | core.PermList})
	everything := NewUser("w", "wildcard", nil).WithAllBuckets(core.PermAll)

	for _, tc := range []struct {
		name       string
		user       *User
		key        string
		want       core.PermissionSet
		wantBucket string
		wantReason string
		ok         bool
	}{
		{"granted bucket", reader, "photos/a.jpg", core.PermRead, "photos", "", true},
		{"missing permission", reader, "photos/a.jpg", core.PermDelete, "photos", ReasonPermission, false},
		{"ungranted bucket", reader, "logs/x", core.PermRead, "logs", ReasonNoGrant, false},
		{"no bucket without wildcard", reader, "", core.PermList, "", ReasonNoBucket, false},
		{"partial name without wildcard", reader, "pho", core.PermList, "", ReasonNoBucket, false},
		{"no bucket with wildcard", everything, "", core.PermDelete, "", "", true},
		{"nil user", nil, "photos/a.jpg", core.PermRead, "photos", ReasonNoGrant, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bucket, reason, ok := AuthorizeKey(tc.user, tc.key, tc.want)
			if bucket != tc.wantBucket || reason != tc.wantReason || ok != tc.ok {
				t.Errorf("AuthorizeKey(%q) = %q, %q, %v; want %q, %q, %v",
					tc.key, bucket, reason, ok, tc.wantBucket, tc.wantReason, tc.ok)
			}
		})
	}
}

// TestUserByAccessKey verifies a key resolves to its user and an unknown key
// resolves to nothing.
func TestUserByAccessKey(t *testing.T) {
	t.Parallel()
	br := &BucketRegistry{byAccessKey: map[string]entry{
		"AKIAKNOWN": {secret: "s", user: NewUser("u1", "one", nil)},
	}}
	if u, ok := br.UserByAccessKey("AKIAKNOWN"); !ok || u.ID != "u1" {
		t.Errorf("UserByAccessKey(known) = %v, %v; want u1, true", u, ok)
	}
	if _, ok := br.UserByAccessKey("AKIANOPE"); ok {
		t.Error("UserByAccessKey resolved an unknown key")
	}
}
