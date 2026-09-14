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

// User is one identity in the merged view, with the buckets it reaches.
//
// A config-declared credential has no user row, so the merge synthesises one
// reaching the single bucket that declared it. Its id is derived from the access
// key, which makes it stable across restarts - an audit record naming it means
// the same thing tomorrow.
// Admin holds the control-plane grants, keyed on the resource they name rather
// than flattened the way Grants is: a backend wildcard cannot be expanded at
// publish time, because the backends a request may name come from config rather
// than from this view.
//
// AllBuckets is what a bucket wildcard carries, kept beside the expansion in
// Grants rather than replaced by it. The expansion answers which buckets exist
// for this identity; the wildcard answers whether it may act on one the
// expansion does not name - a bucket created since, or an operation spanning
// the whole namespace.
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
// be replaced or revoked while its siblings keep working.
//
// Secret is carried because the request path needs the literal value to repeat
// the client's SigV4 key derivation. Nothing that renders a credential to an
// operator may include it.
type Credential struct {
	AccessKeyID string
	UserID      string
	Secret      string
	Label       string
	Source      Source
}

// View is the whole of what a deployment has declared, from both sources.
type View struct {
	Buckets     []Bucket
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
// administers the deployment.
//
// It is an ordinary user holding every permission on every resource, which is
// what lets the request path authorize it the way it authorizes anyone else. A
// deployment that declares none simply has no such user, and administers itself
// through credentials the store holds.
//
// The bucket half is expanded across what either source declares, matching how
// a stored bucket wildcard is published; the wildcard it also carries is what
// reaches a bucket created since.
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
// is dropped and reported rather than merged.
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
// the enabled credentials that prove them.
//
// A disabled credential is left out entirely, which is what makes disabling one
// take effect: nothing downstream learns the access key, so it authenticates
// nothing while its row survives for the record of what it did.
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

// grantsByUser indexes each user's granted buckets and the permissions each
// grant carries, reporting any grant naming a bucket neither source declares.
//
// A grant can outlive the bucket it names - a bucket leaves the config file
// while the grant stays behind - so a dangling one is reported and skipped
// rather than treated as a failure to start.
//
// Two grants naming one bucket union rather than the later replacing the
// earlier. The schema keys on (user, resource) so this cannot arise today, but
// resolving a duplicate by dropping permissions an operator wrote is the wrong
// direction to fail if it ever can.
//
// A bucket wildcard is expanded here against every declared bucket rather than
// matched at request time, so the hot path stays one map read. Creating a bucket
// republishes, which is what keeps the expansion from going stale.
//
// A named grant replaces the wildcard for that bucket rather than adding to it,
// so an operator can hold broad access and still carve one bucket down to
// read-only. Two grants naming the same bucket union, since neither is more
// specific than the other.
//
// Only bucket grants are indexed here. Backend and instance grants authorize the
// control plane, which this lookup has no question to answer about.
func grantsByUser(grants []core.Grant, declared map[string]struct{}) (
	map[string]map[string]core.PermissionSet, map[string]core.PermissionSet, []Notice,
) {
	reach := make(map[string]map[string]core.PermissionSet)
	wildcard := expandWildcardGrants(grants, declared, reach)
	notices := applyNamedGrants(grants, declared, reach)
	return reach, wildcard, notices
}

// expandWildcardGrants writes each bucket wildcard across every declared
// bucket, which is the pass the named grants then narrow.
// Returns what each user's wildcard carries, which the expansion cannot express
// on its own: a listing needs the buckets named, and authorizing an operation
// that spans them - the empty prefix is the whole namespace - needs to know the
// caller may reach a bucket nobody has declared yet.
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
// they name.
//
// Kept whole rather than expanded the way bucket wildcards are: the backends a
// request may name come from config, which this view does not hold, so the
// wildcard is resolved when the request is authorized instead.
//
// Nothing is reported for a grant naming a backend no deployment serves. The
// grant is refused when it is written, and a backend leaving config later is the
// same case as a bucket leaving it - the grant outlives it and authorizes
// nothing.
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

// ConfigUserID names the user a config-declared credential resolves to.
//
// The access key is what identifies it, which makes the id stable across
// restarts and across reordering the bucket's credential list - an audit record
// naming it means the same thing tomorrow.
func ConfigUserID(accessKeyID string) string {
	return "config:" + accessKeyID
}
