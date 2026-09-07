// -------------------------------------------------------------------------------
// Provisioning - Reading the Store
//
// Author: Alex Freidah
//
// Loads every provisioning table into one snapshot so the merge runs against a
// single consistent picture rather than four queries a write could land between.
// -------------------------------------------------------------------------------

package provisioning

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Reader is the half of the provisioning store this package needs. Declared here
// rather than taken whole so a test can supply four slices without standing up a
// store that can also write.
type Reader interface {
	ListBuckets(ctx context.Context) ([]core.Bucket, error)
	ListUsers(ctx context.Context) ([]core.User, error)
	ListCredentials(ctx context.Context) ([]core.Credential, error)
	ListGrants(ctx context.Context) ([]core.Grant, error)
}

// Load reads the store's provisioning tables.
//
// A read that fails is returned rather than degraded into an empty snapshot:
// merging against nothing would answer 403 to every caller the store entitles,
// which is worse than not starting.
func Load(ctx context.Context, r Reader) (*Snapshot, error) {
	var s Snapshot
	var err error
	if s.Buckets, err = r.ListBuckets(ctx); err != nil {
		return nil, fmt.Errorf("read provisioned buckets: %w", err)
	}
	if s.Users, err = r.ListUsers(ctx); err != nil {
		return nil, fmt.Errorf("read provisioned users: %w", err)
	}
	if s.Credentials, err = r.ListCredentials(ctx); err != nil {
		return nil, fmt.Errorf("read provisioned credentials: %w", err)
	}
	if s.Grants, err = r.ListGrants(ctx); err != nil {
		return nil, fmt.Errorf("read provisioned grants: %w", err)
	}
	return &s, nil
}

// LoadMerged reads the store and folds config in, which is the whole of what
// both the request-time registry and an operator's listing are built from.
func LoadMerged(ctx context.Context, r Reader, cfgBuckets []config.BucketConfig) (View, error) {
	s, err := Load(ctx, r)
	if err != nil {
		return View{}, err
	}
	return Merge(cfgBuckets, s), nil
}
