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

// Grant is a user's access to one bucket, and the permissions that access
// carries.
//
// BucketName may name a bucket the config file declares rather than one the
// store holds, which is why it is not a foreign key.
type Grant struct {
	UserID      string
	BucketName  string
	Permissions PermissionSet
	CreatedAt   time.Time
}
