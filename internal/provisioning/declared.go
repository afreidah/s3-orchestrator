// -------------------------------------------------------------------------------
// Provisioning - The Live Declared Bucket Set
//
// Author: Alex Freidah
//
// What every subsystem outside the auth path asks when it needs to know whether
// a bucket exists, or what one carries. Assembly writes the merged set here
// whenever it rebuilds, so a bucket created through the API is visible to the
// admin object endpoints, the browser CORS policy, S3 admission, the write path,
// the reconciler and the web UI at the same moment it becomes reachable over S3.
//
// Held behind an atomic pointer and swapped whole, matching how the credential
// registry is published: readers on request goroutines never take a lock, and a
// reader mid-call keeps the consistent set it started with.
// -------------------------------------------------------------------------------

package provisioning

import (
	"slices"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// Declared is the merged bucket set, live. Registry assembly stores it at
// startup, after each reload, and after each provisioning change. The zero
// value is usable and reports nothing declared, which is what a process holds
// before its first assembly.
type Declared struct {
	set        atomic.Pointer[declaredSet]
	deployment atomic.Pointer[deployment]
}

// declaredSet is one published bucket set, indexed by name so the lookups the
// request path makes do not scan it. forms and factors hold the stored form
// and replication factor of each bucket that sets its own.
type declaredSet struct {
	buckets []Bucket
	byName  map[string]int
	forms   map[string]core.StoredFlags
	factors map[string]int
}

// deployment is where a bucket without settings of its own takes them: the
// running configuration, and whether the process built an encryptor, which
// follows the configuration it started with rather than a reload.
type deployment struct {
	live     *config.Live
	encrypts bool
}

// NewDeclared builds an empty holder.
func NewDeclared() *Declared {
	return &Declared{}
}

// Store publishes buckets as the declared set, replacing the one before it.
// The slice is kept, not copied, so the caller must not modify it after.
func (d *Declared) Store(buckets []Bucket) {
	s := &declaredSet{
		buckets: buckets, byName: make(map[string]int, len(buckets)),
		forms: map[string]core.StoredFlags{}, factors: map[string]int{},
	}
	for i := range buckets {
		b := &buckets[i]
		s.byName[b.Name] = i
		if b.StoredForm != nil {
			// Validated where the bucket was declared, so every name parses.
			s.forms[b.Name], _ = core.ParseStoredFlags(*b.StoredForm)
		}
		if b.ReplicationFactor > 0 {
			s.factors[b.Name] = b.ReplicationFactor
		}
	}
	d.set.Store(s)
}

// UseDeployment names where a bucket without settings of its own takes them:
// live, read on each lookup so a reload reaches them, and whether the process
// encrypts. Until it is called such a bucket stores no layer and keeps no
// factor.
func (d *Declared) UseDeployment(live *config.Live, encrypts bool) {
	d.deployment.Store(&deployment{live: live, encrypts: encrypts})
}

// DeploymentForm returns the stored form of a bucket without its own: the
// layers compression and integrity enable, and encrypted when the process
// encrypts. Before UseDeployment it is no layer at all.
func (d *Declared) DeploymentForm() core.StoredFlags {
	dep := d.dep()
	if dep == nil {
		return 0
	}
	cfg := dep.live.Load()
	var form core.StoredFlags
	if cfg.Compression.Enabled {
		form |= core.StoredCompressed
	}
	if dep.encrypts {
		form |= core.StoredEncrypted
	}
	if cfg.Integrity.Enabled {
		form |= core.StoredHashed
	}
	return form
}

// load returns the published set, or nil before the first Store or when d
// itself is nil.
func (d *Declared) load() *declaredSet {
	if d == nil {
		return nil
	}
	return d.set.Load()
}

// Buckets returns the declared set. The slice is the stored one and must not be
// modified; callers that need to keep it past the next swap copy it.
func (d *Declared) Buckets() []Bucket {
	if s := d.load(); s != nil {
		return s.buckets
	}
	return nil
}

// Names lists the declared bucket names, in the order assembly produced: config
// first, then the store.
func (d *Declared) Names() []string {
	buckets := d.Buckets()
	out := make([]string, 0, len(buckets))
	for i := range buckets {
		out = append(out, buckets[i].Name)
	}
	return out
}

// Contains reports whether a bucket of that name is declared.
func (d *Declared) Contains(name string) bool {
	_, ok := d.lookup(name)
	return ok
}

// HasPrefix reports whether an object key names a declared bucket, which is the
// check that keeps an admin object call from addressing a namespace nothing
// declares. A key equal to a bucket name with no trailing slash names no
// object, so it does not match.
func (d *Declared) HasPrefix(key string) bool {
	bucket, _ := internalkey.Split(key)
	return len(key) > len(bucket) && d.Contains(bucket)
}

// MaxMultipartUploads returns the limit on a bucket's active multipart
// uploads, zero when it has none or is not declared.
func (d *Declared) MaxMultipartUploads(bucket string) int {
	if b, ok := d.lookup(bucket); ok {
		return b.MaxMultipartUploads
	}
	return 0
}

// Form returns the layers a new object in bucket is stored with: its own
// stored form when it sets one, and the deployment's otherwise.
func (d *Declared) Form(bucket string) core.StoredFlags {
	if s := d.load(); s != nil {
		if form, ok := s.forms[bucket]; ok {
			return form
		}
	}
	return d.DeploymentForm()
}

// Factors returns how many copies each key is kept at: its bucket's own
// replication factor when it sets one, and the deployment's otherwise.
func (d *Declared) Factors() core.ReplicationFactors {
	var f core.ReplicationFactors
	if dep := d.dep(); dep != nil {
		f.Default = dep.live.Load().Replication.Factor
	}
	if s := d.load(); s != nil {
		f.Buckets = s.factors
	}
	return f
}

// Wanting scopes a listing to the buckets whose new objects carry layer, so a
// pass adding it to existing copies leaves the others alone. When the
// deployment's form carries it, that is every bucket but those whose own form
// leaves it out; otherwise only those whose own form names it.
func (d *Declared) Wanting(layer core.StoredFlags) core.BucketScope {
	scope := core.BucketScope{Only: !d.DeploymentForm().Has(layer)}
	if s := d.load(); s != nil {
		for name, form := range s.forms {
			if form.Has(layer) == scope.Only {
				scope.Names = append(scope.Names, name)
			}
		}
	}
	slices.Sort(scope.Names)
	return scope
}

// lookup returns the declared bucket of that name.
func (d *Declared) lookup(name string) (*Bucket, bool) {
	s := d.load()
	if s == nil {
		return nil, false
	}
	i, ok := s.byName[name]
	if !ok {
		return nil, false
	}
	return &s.buckets[i], true
}

// dep returns where a bucket without settings of its own takes them, or nil
// before UseDeployment or when d itself is nil.
func (d *Declared) dep() *deployment {
	if d == nil {
		return nil
	}
	return d.deployment.Load()
}
