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
type User struct {
	ID      string
	Name    string
	Buckets []string
	Grants  map[string]core.PermissionSet
	Source  Source
}

// Credential is one proof of a user, by keypair, by legacy proxy token, or by
// both - a config-declared credential may carry either or each, and both prove
// the same user.
//
// Secret and Token are carried because the request path needs the literal values
// to repeat the client's SigV4 key derivation and to compare a token. Nothing
// that renders a credential to an operator may include either.
type Credential struct {
	AccessKeyID string
	UserID      string
	Secret      string
	Token       string
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
func Merge(cfgBuckets []config.BucketConfig, s *Snapshot) View {
	var v View
	declared := mergeBuckets(&v, cfgBuckets, s.Buckets)
	mergeConfigUsers(&v, cfgBuckets)
	mergeStoredUsers(&v, s, declared)
	return v
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
			id := ConfigUserID(bkt.Name, j, cred.AccessKeyID)
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
			out := Credential{UserID: id, Token: cred.Token, Source: SourceConfig}
			if cred.AccessKeyID != "" && cred.SecretAccessKey != "" {
				out.AccessKeyID = cred.AccessKeyID
				out.Secret = cred.SecretAccessKey
			}
			v.Credentials = append(v.Credentials, out)
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
	reach, notices := grantsByUser(s.Grants, declared)
	v.Notices = append(v.Notices, notices...)

	known := make(map[string]struct{}, len(s.Users))
	for i := range s.Users {
		u := &s.Users[i]
		known[u.ID] = struct{}{}
		grants := reach[u.ID]
		v.Users = append(v.Users, User{
			ID:      u.ID,
			Name:    u.Name,
			Buckets: sortedKeys(grants),
			Grants:  grants,
			Source:  SourceStore,
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
// Only bucket grants are indexed here. A grant on a backend or on the fleet is
// storable and carries no meaning yet: what the control plane grants is its own
// action set, over a resource this lookup has no question to answer about.
func grantsByUser(grants []core.Grant, declared map[string]struct{}) (map[string]map[string]core.PermissionSet, []Notice) {
	reach := make(map[string]map[string]core.PermissionSet)
	var notices []Notice
	for i := range grants {
		g := &grants[i]
		if g.Resource.Kind != core.ResourceBucket {
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
		reach[g.UserID][g.Resource.Name] |= g.Permissions
	}
	return reach, notices
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

// ConfigUserID names the user a config-declared credential resolves to. The
// access key is the stable choice where there is one; a token-only credential
// has no public identifier, so it falls back to its position, which is stable as
// long as the bucket's credential list is.
func ConfigUserID(bucketName string, idx int, accessKeyID string) string {
	if accessKeyID != "" {
		return "config:" + accessKeyID
	}
	return fmt.Sprintf("config:%s:%d", bucketName, idx)
}
