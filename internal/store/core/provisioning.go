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

// ResourceKind says what a grant names. The control plane has no bucket, so
// draining a backend or provisioning a user needs a resource that is not one.
//
// ResourceInstance carries no name: there is one instance, so the name is stored
// empty.
type ResourceKind string

// The three kinds a grant may name.
const (
	ResourceBucket   ResourceKind = "bucket"
	ResourceBackend  ResourceKind = "backend"
	ResourceInstance ResourceKind = "instance"
)

// ResourceWildcard is the name matching every resource of a kind, including ones
// created later. It is how an operator is granted a fleet rather than a list
// that goes stale the next time someone adds a bucket.
//
// Reserved rather than escapable: S3 bucket names cannot contain it and backend
// names come from config, so nothing legitimate is shadowed.
const ResourceWildcard = "*"

// Resource is what a grant is over: a kind and the name of one thing of that
// kind, the wildcard for all of them, or no name for the instance.
//
// Name may identify a bucket the config file declares rather than one the store
// holds, which is why neither half is a foreign key.
type Resource struct {
	Kind ResourceKind
	Name string
}

// BucketResource names one bucket, which is what every grant written before the
// control plane had its own permissions is.
func BucketResource(name string) Resource {
	return Resource{Kind: ResourceBucket, Name: name}
}

// IsWildcard reports whether this resource stands for every one of its kind.
func (r Resource) IsWildcard() bool {
	return r.Name == ResourceWildcard
}

// String renders the resource the way a grant listing and an audit entry name
// it, so the two read alike.
func (r Resource) String() string {
	if r.Kind == ResourceInstance {
		return string(r.Kind)
	}
	return string(r.Kind) + ":" + r.Name
}

// Grant is a user's access to one resource, and the permissions that access
// carries.
//
// One permission set covers both planes: a data-plane bit on a backend grant
// and an admin bit on a bucket grant are refused when the grant is written, so
// the type stays single and the resource decides what is meaningful.
type Grant struct {
	UserID      string
	Resource    Resource
	Permissions PermissionSet
	CreatedAt   time.Time
}
