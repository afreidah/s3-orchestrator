// -------------------------------------------------------------------------------
// Provisioning - The Merged View
//
// Author: Alex Freidah
//
// Folds the store's rows in beside what the config file declares and reports
// what the merge found. Config is authoritative for any name it carries, and a
// credential the config file declares becomes a user reaching the one bucket
// that declared it, so both sources produce the same shape and nothing
// downstream has a second case to handle.
// -------------------------------------------------------------------------------

package provisioning

import (
	"fmt"
	"slices"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Source says which of the two places an entry was declared in. Config-declared
// entries are read-only: the provisioning API refuses to modify them, because an
// operator reading the config file has to be able to trust what it says.
type Source string

// SourceConfig and SourceStore are the two places an entry comes from.
const (
	SourceConfig Source = "config"
	SourceStore  Source = "store"
)

// RootUserID names the user the configured root credential resolves to. It is
// fixed rather than derived from the access key, so rotating that key leaves
// every audit record naming the same identity.
const RootUserID = "config:root"

// rootUserName is what an operator's listing calls the root user.
const rootUserName = "root"

// Snapshot is what the store holds, read whole so the merge runs against one
// consistent picture rather than four queries a write could land between.
type Snapshot struct {
	Buckets     []core.Bucket
	Users       []core.User
	Credentials []core.Credential
	Grants      []core.Grant
}

// Bucket is one virtual bucket in the merged view.
type Bucket struct {
	Name                string
	MaxMultipartUploads int
	CORS                []config.CORSRule
	Source              Source
}

// User is one identity in the merged view, with the buckets it reaches. A
// config-declared credential has no user row, so the merge synthesises one
// reaching the bucket that declared it, with an id from ConfigUserID.
//
// Admin holds the control-plane grants keyed on the resource they name, since
// a backend wildcard can only be resolved against config at request time.
// AllBuckets is what a bucket wildcard carries beside its expansion in Grants,
// so a bucket created since, or an operation spanning the whole namespace,
// can still be authorized.
type User struct {
	ID         string
	Name       string
	Buckets    []string
	Grants     map[string]core.PermissionSet
	AllBuckets core.PermissionSet
	Admin      map[core.Resource]core.PermissionSet
	Source     Source
}

// Credential is one keypair proving a user. A user may hold several, so one can
// be replaced or revoked while its siblings keep working. Secret is the literal
// value SigV4 verification needs; nothing that renders a credential to an
// operator may include it.
type Credential struct {
	AccessKeyID string
	UserID      string
	Secret      string
	Label       string
	Source      Source
}

// View is the whole of what a deployment has declared, from both sources.
//
// Shadowed holds stored buckets that config declares too. Config wins, so these
// are left out of Buckets and nothing authorizes or routes against them. A
// bucket moves out of the config file without a gap by writing the store row
// first, then removing the config entry.
type View struct {
	Buckets     []Bucket
	Shadowed    []Bucket
	Users       []User
	Credentials []Credential
	Notices     []Notice
}

// -------------------------------------------------------------------------
// MERGE
// -------------------------------------------------------------------------

// Merge folds the store's rows in beside what config declares.
//
// Pure, so the precedence and grant-joining rules can be exercised without a
// store or an injector.
func Merge(cfgBuckets []config.BucketConfig, auth config.AuthConfig, s *Snapshot) View {
	var v View
	declared := mergeBuckets(&v, cfgBuckets, s.Buckets)
	mergeRootUser(&v, auth, declared)
	mergeConfigUsers(&v, cfgBuckets)
	mergeStoredUsers(&v, s, declared)
	return v
}

// mergeRootUser turns the configured root credential into the user that
// administers the deployment: an ordinary user holding every permission on
// every resource. Its bucket grants are expanded across every declared bucket,
// and its wildcard reaches buckets created since.
func mergeRootUser(v *View, auth config.AuthConfig, declared map[string]struct{}) {
	if !auth.HasRoot() {
		return
	}
	grants := make(map[string]core.PermissionSet, len(declared))
	for bucket := range declared {
		grants[bucket] = core.PermAll
	}
	v.Users = append(v.Users, User{
		ID:         RootUserID,
		Name:       rootUserName,
		Buckets:    sortedKeys(grants),
		Grants:     grants,
		AllBuckets: core.PermAll,
		Admin: map[core.Resource]core.PermissionSet{
			{Kind: core.ResourceOrchestrator}:                         core.PermAdminAll,
			{Kind: core.ResourceBackend, Name: core.ResourceWildcard}: core.PermAdminAll,
		},
		Source: SourceConfig,
	})
	v.Credentials = append(v.Credentials, Credential{
		AccessKeyID: auth.Root.AccessKeyID,
		UserID:      RootUserID,
		Secret:      auth.Root.SecretAccessKey,
		Label:       rootUserName,
		Source:      SourceConfig,
	})
}

// mergeBuckets appends both sources' buckets, config first, and returns the set
// of names either one declares. A stored bucket whose name config also carries
// is reported and set aside in Shadowed rather than merged.
func mergeBuckets(v *View, cfgBuckets []config.BucketConfig, stored []core.Bucket) map[string]struct{} {
	declared := make(map[string]struct{}, len(cfgBuckets)+len(stored))
	for i := range cfgBuckets {
		b := &cfgBuckets[i]
		declared[b.Name] = struct{}{}
		v.Buckets = append(v.Buckets, Bucket{
			Name:                b.Name,
			MaxMultipartUploads: b.MaxMultipartUploads,
			CORS:                b.CORS,
			Source:              SourceConfig,
		})
	}
	for i := range stored {
		b := &stored[i]
		if _, ok := declared[b.Name]; ok {
			v.Notices = append(v.Notices, Notice{
				Kind:   NoticeBucketShadowed,
				Detail: fmt.Sprintf("stored bucket %q is shadowed by a config bucket", b.Name),
			})
			v.Shadowed = append(v.Shadowed, Bucket{
				Name:                b.Name,
				MaxMultipartUploads: b.MaxMultipartUploads,
				CORS:                b.CORS,
				Source:              SourceStore,
			})
			continue
		}
		declared[b.Name] = struct{}{}
		v.Buckets = append(v.Buckets, Bucket{
			Name:                b.Name,
			MaxMultipartUploads: b.MaxMultipartUploads,
			CORS:                b.CORS,
			Source:              SourceStore,
		})
	}
	return declared
}

// mergeConfigUsers turns each config-declared credential into a user reaching
// the bucket that declared it.
//
// A credential carrying both a keypair and a token stays one credential proving
// one user, so either proof attributes the same actor.
func mergeConfigUsers(v *View, cfgBuckets []config.BucketConfig) {
	for i := range cfgBuckets {
		bkt := &cfgBuckets[i]
		for j := range bkt.Credentials {
			cred := &bkt.Credentials[j]
			id := ConfigUserID(cred.AccessKeyID)
			// Full access, matching what a config credential has always
			// carried. The config file has no syntax for narrowing it, and
			// inventing one here would split the same idea across two places.
			v.Users = append(v.Users, User{
				ID:      id,
				Name:    bkt.Name,
				Buckets: []string{bkt.Name},
				Grants:  map[string]core.PermissionSet{bkt.Name: core.PermAll},
				Source:  SourceConfig,
			})
			v.Credentials = append(v.Credentials, Credential{
				AccessKeyID: cred.AccessKeyID,
				UserID:      id,
				Secret:      cred.SecretAccessKey,
				Source:      SourceConfig,
			})
		}
	}
}

// mergeStoredUsers appends the store's users with the buckets they reach, and
// the enabled credentials that prove them. A disabled credential is left out,
// so it authenticates nothing while its row survives.
func mergeStoredUsers(v *View, s *Snapshot, declared map[string]struct{}) {
	reach, wildcard, notices := grantsByUser(s.Grants, declared)
	v.Notices = append(v.Notices, notices...)
	admin := adminGrantsByUser(s.Grants)

	known := make(map[string]struct{}, len(s.Users))
	for i := range s.Users {
		u := &s.Users[i]
		known[u.ID] = struct{}{}
		grants := reach[u.ID]
		v.Users = append(v.Users, User{
			ID:         u.ID,
			Name:       u.Name,
			Buckets:    sortedKeys(grants),
			Grants:     grants,
			AllBuckets: wildcard[u.ID],
			Admin:      admin[u.ID],
			Source:     SourceStore,
		})
	}

	for i := range s.Credentials {
		c := &s.Credentials[i]
		if c.Disabled {
			continue
		}
		if _, ok := known[c.UserID]; !ok {
			continue
		}
		v.Credentials = append(v.Credentials, Credential{
			AccessKeyID: c.AccessKeyID,
			UserID:      c.UserID,
			Secret:      c.Secret,
			Label:       c.Label,
			Source:      SourceStore,
		})
	}
}

// grantsByUser indexes each user's bucket grants. Wildcards are expanded across
// the declared buckets so the hot path is one map read, and a named grant
// replaces the wildcard for its bucket so one bucket can be narrowed. A grant
// naming an undeclared bucket is reported as a notice and skipped.
func grantsByUser(grants []core.Grant, declared map[string]struct{}) (
	map[string]map[string]core.PermissionSet, map[string]core.PermissionSet, []Notice,
) {
	reach := make(map[string]map[string]core.PermissionSet)
	wildcard := expandWildcardGrants(grants, declared, reach)
	notices := applyNamedGrants(grants, declared, reach)
	return reach, wildcard, notices
}

// expandWildcardGrants writes each bucket wildcard across every declared
// bucket, which the named grants then narrow. It returns each user's wildcard
// permissions, which authorize buckets not yet declared.
func expandWildcardGrants(
	grants []core.Grant, declared map[string]struct{}, reach map[string]map[string]core.PermissionSet,
) map[string]core.PermissionSet {
	wildcard := make(map[string]core.PermissionSet)
	for i := range grants {
		g := &grants[i]
		if g.Resource.Kind != core.ResourceBucket || !g.Resource.IsWildcard() {
			continue
		}
		wildcard[g.UserID] |= g.Permissions
		if reach[g.UserID] == nil {
			reach[g.UserID] = make(map[string]core.PermissionSet)
		}
		for bucket := range declared {
			reach[g.UserID][bucket] |= g.Permissions
		}
	}
	return wildcard
}

// applyNamedGrants lays each named bucket grant over the expanded wildcard,
// reporting the ones naming a bucket neither source declares.
func applyNamedGrants(grants []core.Grant, declared map[string]struct{}, reach map[string]map[string]core.PermissionSet) []Notice {
	var notices []Notice
	named := make(map[string]map[string]bool)
	for i := range grants {
		g := &grants[i]
		if g.Resource.Kind != core.ResourceBucket || g.Resource.IsWildcard() {
			continue
		}
		if _, ok := declared[g.Resource.Name]; !ok {
			notices = append(notices, Notice{
				Kind: NoticeDanglingGrant,
				Detail: fmt.Sprintf("grant names bucket %q, which neither config nor the store declares",
					g.Resource.Name),
			})
			continue
		}
		if reach[g.UserID] == nil {
			reach[g.UserID] = make(map[string]core.PermissionSet)
		}
		if named[g.UserID] == nil {
			named[g.UserID] = make(map[string]bool)
		}
		// The first named grant displaces whatever the wildcard put here; a
		// second one unions with the first.
		if !named[g.UserID][g.Resource.Name] {
			reach[g.UserID][g.Resource.Name] = 0
			named[g.UserID][g.Resource.Name] = true
		}
		reach[g.UserID][g.Resource.Name] |= g.Permissions
	}
	return notices
}

// adminGrantsByUser indexes each user's control-plane grants by the resource
// they name. Wildcards are not expanded because the backends come from config,
// which this view does not hold. A grant naming a backend that has left config
// is kept and authorizes nothing.
func adminGrantsByUser(grants []core.Grant) map[string]map[core.Resource]core.PermissionSet {
	admin := make(map[string]map[core.Resource]core.PermissionSet)
	for i := range grants {
		g := &grants[i]
		if g.Resource.Kind == core.ResourceBucket {
			continue
		}
		if admin[g.UserID] == nil {
			admin[g.UserID] = make(map[core.Resource]core.PermissionSet)
		}
		admin[g.UserID][g.Resource] |= g.Permissions
	}
	return admin
}

// sortedKeys lists the buckets a grant map names, in order, which is what a
// ListBuckets response enumerates and what an operator's listing renders.
func sortedKeys(grants map[string]core.PermissionSet) []string {
	out := make([]string, 0, len(grants))
	for name := range grants {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// ConfigUserID names the user a config-declared credential resolves to. It is
// derived from the access key so it is stable across restarts and reordering.
func ConfigUserID(accessKeyID string) string {
	return "config:" + accessKeyID
}
