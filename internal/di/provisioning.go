// -------------------------------------------------------------------------------
// Bucket Registry Assembly
//
// Author: Alex Freidah
//
// Joins the store's provisioning rows with what the config file declares into
// the two lists the bucket registry takes. This is the only place both sources
// are visible at once: the auth package never learns the store exists, and the
// store never learns config does.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/auth"
)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// AssembleBucketRegistry builds the bucket registry from both sources a
// deployment declares credentials in: the buckets in cfg, and the users the
// store holds. Exported because the reload hook has to assemble the same way a
// boot does - rebuilding from the config file alone would drop every
// API-created bucket on each SIGHUP.
//
// A store that cannot be read fails rather than falling back to config alone:
// serving with half the credentials answers 403 to callers that are entitled,
// which is worse than not starting.
func AssembleBucketRegistry(ctx context.Context, i do.Injector, cfg *config.Config) (*auth.BucketRegistry, error) {
	store, err := do.Invoke[core.ProvisioningStore](i)
	if err != nil {
		return nil, err
	}

	p, err := readProvisioned(ctx, store)
	if err != nil {
		return nil, err
	}
	merged := mergeProvisioned(cfg.Buckets, &p)

	registry, err := auth.NewBucketRegistry(merged.Buckets, merged.Credentials)
	if err != nil {
		return nil, err
	}
	logAssemblyNotices(ctx, append(merged.Notices, registry.Notices()...))
	return registry, nil
}

// logAssemblyNotices reports what assembly served through. Warn rather than
// error: each one describes a state the fleet is running in, not a failure to
// reach it.
func logAssemblyNotices(ctx context.Context, notices []auth.Notice) {
	for _, n := range notices {
		slog.WarnContext(ctx, "bucket registry assembly",
			logfmt.Component("di"),
			"kind", n.Kind,
			"detail", n.Detail,
		)
	}
}

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// provisioned is what the store holds, read whole so the join runs against one
// consistent picture rather than four queries a write could land between.
type provisioned struct {
	Buckets     []core.Bucket
	Users       []core.User
	Credentials []core.Credential
	Grants      []core.Grant
}

// assembled is what the registry is built from: the bucket list both the auth
// and CORS registries consume, the keypairs the store contributes, and what the
// join found worth reporting.
type assembled struct {
	Buckets     []config.BucketConfig
	Credentials []auth.StoredCredential
	Notices     []auth.Notice
}

// -------------------------------------------------------------------------
// ASSEMBLY
// -------------------------------------------------------------------------

// readProvisioned loads every provisioning table.
func readProvisioned(ctx context.Context, store core.ProvisioningStore) (provisioned, error) {
	var p provisioned
	var err error
	if p.Buckets, err = store.ListBuckets(ctx); err != nil {
		return provisioned{}, fmt.Errorf("read provisioned buckets: %w", err)
	}
	if p.Users, err = store.ListUsers(ctx); err != nil {
		return provisioned{}, fmt.Errorf("read provisioned users: %w", err)
	}
	if p.Credentials, err = store.ListCredentials(ctx); err != nil {
		return provisioned{}, fmt.Errorf("read provisioned credentials: %w", err)
	}
	if p.Grants, err = store.ListGrants(ctx); err != nil {
		return provisioned{}, fmt.Errorf("read provisioned grants: %w", err)
	}
	return p, nil
}

// mergeProvisioned folds the store's rows in beside what config declares.
//
// Config is authoritative for any bucket name it carries, so a stored bucket of
// the same name is dropped rather than merged: an operator reading the config
// file has to be able to trust what it says.
//
// A stored bucket contributes no credentials. Its users reach it through grants,
// and the Credentials field on the converted entry stays empty for that reason -
// nothing should read it.
//
// Pure, so the precedence and grant-joining rules can be exercised without a
// store or an injector.
func mergeProvisioned(cfgBuckets []config.BucketConfig, p *provisioned) assembled {
	out := assembled{Buckets: cfgBuckets}

	declared := make(map[string]struct{}, len(cfgBuckets)+len(p.Buckets))
	for i := range cfgBuckets {
		declared[cfgBuckets[i].Name] = struct{}{}
	}
	for i := range p.Buckets {
		b := &p.Buckets[i]
		if _, ok := declared[b.Name]; ok {
			out.Notices = append(out.Notices, auth.Notice{
				Kind:   auth.NoticeCredentialShadowed,
				Detail: fmt.Sprintf("stored bucket %q is shadowed by a config bucket", b.Name),
			})
			continue
		}
		declared[b.Name] = struct{}{}
		out.Buckets = append(out.Buckets, config.BucketConfig{
			Name:                b.Name,
			MaxMultipartUploads: b.MaxMultipartUploads,
			CORS:                b.CORS,
		})
	}

	reach, notices := grantsByUser(p.Grants, declared)
	out.Notices = append(out.Notices, notices...)
	out.Credentials = storedCredentials(p, reach)
	return out
}

// grantsByUser indexes each user's granted buckets, reporting any grant naming a
// bucket neither source declares.
//
// A grant can outlive the bucket it names - a bucket leaves the config file
// while the grant stays behind - so a dangling one is reported and skipped
// rather than treated as a failure to start.
func grantsByUser(grants []core.Grant, declared map[string]struct{}) (map[string][]string, []auth.Notice) {
	reach := make(map[string][]string)
	var notices []auth.Notice
	for i := range grants {
		g := &grants[i]
		if _, ok := declared[g.BucketName]; !ok {
			notices = append(notices, auth.Notice{
				Kind: auth.NoticeDanglingGrant,
				Detail: fmt.Sprintf("grant names bucket %q, which neither config nor the store declares",
					g.BucketName),
			})
			continue
		}
		reach[g.UserID] = append(reach[g.UserID], g.BucketName)
	}
	return reach, notices
}

// storedCredentials pairs each enabled keypair with the user it proves.
//
// A disabled credential is left out entirely, which is what makes disabling one
// take effect: the registry never learns the access key, so it authenticates
// nothing while its row survives for the record of what it did.
//
// A credential whose user has no grants still resolves, and reaches nothing. It
// authenticates and is refused on every bucket, which is a clearer answer than
// a failed signature for an operator who granted nothing yet.
func storedCredentials(p *provisioned, reach map[string][]string) []auth.StoredCredential {
	users := make(map[string]*auth.User, len(p.Users))
	for i := range p.Users {
		u := &p.Users[i]
		users[u.ID] = auth.NewUser(u.ID, u.Name, reach[u.ID])
	}

	out := make([]auth.StoredCredential, 0, len(p.Credentials))
	for i := range p.Credentials {
		c := &p.Credentials[i]
		if c.Disabled {
			continue
		}
		u, ok := users[c.UserID]
		if !ok {
			continue
		}
		out = append(out, auth.StoredCredential{
			AccessKeyID: c.AccessKeyID,
			Secret:      c.Secret,
			User:        u,
		})
	}
	return out
}
