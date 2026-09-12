// -------------------------------------------------------------------------------
// Provisioning - Buckets, Users, Credentials and Grants
//
// Author: Alex Freidah
//
// The value types behind the database half of the bucket registry. An access key
// names a credential, a credential belongs to a user, and a user holds a grant
// per bucket it may reach. The registry is assembled from these rows merged with
// the buckets and credentials the config file declares.
// -------------------------------------------------------------------------------

package core

import (
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Bucket is a virtual bucket held in the store rather than declared in config.
//
// MaxMultipartUploads caps how many uploads may be active against the bucket at
// once and zero means unlimited, matching the config field it mirrors. CORS
// carries the browser rules so a bucket behaves the same whichever source
// declared it.
type Bucket struct {
	Name                string
	MaxMultipartUploads int
	CORS                []config.CORSRule
	CreatedAt           time.Time
}

// User is the identity a request is attributed to. Credentials prove a caller is
// one, and grants say which buckets that one reaches.
//
// Name is what an operator calls it and may change. ID is what credentials and
// grants reference, and does not.
type User struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// Credential is one keypair a user authenticates with. A user may hold several,
// so one can be replaced or revoked while its siblings keep working.
//
// Secret is the value SigV4 derives a signing key from, which is why it is read
// back rather than hashed. Disabled stops a credential authenticating while
// keeping the record of what it did.
type Credential struct {
	AccessKeyID string
	UserID      string
	Secret      string
	Label       string
	Disabled    bool
	CreatedAt   time.Time
	LastUsedAt  *time.Time
}

// ResourceKind says what a grant names. The control plane has no bucket, so an
// operation on the fleet or on one backend needs a resource that is not one.
//
// ResourceFleet carries no name: there is one fleet, and the name is part of the
// key rather than nullable, so it is stored as the empty string.
type ResourceKind string

// The three kinds a grant may name.
const (
	ResourceBucket  ResourceKind = "bucket"
	ResourceBackend ResourceKind = "backend"
	ResourceFleet   ResourceKind = "fleet"
)

// Resource is what a grant is over: a kind and the name of the one thing of that
// kind, or no name for the fleet.
//
// Name may identify a bucket the config file declares rather than one the store
// holds, which is why neither half is a foreign key.
type Resource struct {
	Kind ResourceKind
	Name string
}

// BucketResource names one bucket, which is what every grant written before the
// control plane had its own action set is.
func BucketResource(name string) Resource {
	return Resource{Kind: ResourceBucket, Name: name}
}

// Grant is a user's access to one resource, and the permissions that access
// carries.
type Grant struct {
	UserID      string
	Resource    Resource
	Permissions PermissionSet
	CreatedAt   time.Time
}
