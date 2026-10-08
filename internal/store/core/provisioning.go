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
// Its fields mirror the config ones; MaxMultipartUploads of zero means
// unlimited.
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

// Credential is one keypair a user authenticates with; a user may hold several.
// Secret is stored readable rather than hashed because SigV4 derives the
// signing key from it. Disabled stops authentication but keeps the record.
type Credential struct {
	AccessKeyID string
	UserID      string
	Secret      string
	Label       string
	Disabled    bool
	CreatedAt   time.Time
	LastUsedAt  *time.Time
}

// ResourceKind says what a grant names: a bucket, a backend, or the
// orchestrator itself. ResourceOrchestrator carries no name because a
// deployment has only one.
type ResourceKind string

// The three kinds a grant may name.
const (
	ResourceBucket       ResourceKind = "bucket"
	ResourceBackend      ResourceKind = "backend"
	ResourceOrchestrator ResourceKind = "orchestrator"
)

// resourceKindInstance is a stored spelling of ResourceOrchestrator that is
// still accepted, so grants read correctly before the migration has run.
const resourceKindInstance ResourceKind = "instance"

// ParseResourceKind reads the stored or submitted spelling of a kind, accepting
// "instance" for the orchestrator. An empty value means a bucket.
func ParseResourceKind(s string) ResourceKind {
	switch ResourceKind(s) {
	case "":
		return ResourceBucket
	case resourceKindInstance:
		return ResourceOrchestrator
	default:
		return ResourceKind(s)
	}
}

// ResourceWildcard is the name matching every resource of a kind, including ones
// created later. It is reserved with no escape: S3 bucket names cannot contain
// it and backend names come from config.
const ResourceWildcard = "*"

// Resource is what a grant is over: a kind and the name of one thing of that
// kind, the wildcard for all of them, or no name for the orchestrator.
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
	if r.Kind == ResourceOrchestrator {
		return string(r.Kind)
	}
	return string(r.Kind) + ":" + r.Name
}

// Grant is a user's access to one resource and the permissions it carries.
// Permissions invalid for the resource kind are refused when the grant is
// written.
type Grant struct {
	UserID      string
	Resource    Resource
	Permissions PermissionSet
	CreatedAt   time.Time
}
