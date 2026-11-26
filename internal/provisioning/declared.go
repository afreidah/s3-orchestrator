// -------------------------------------------------------------------------------
// Provisioning - The Live Declared Bucket Set
//
// Author: Alex Freidah
//
// What every subsystem outside the auth path asks when it needs to know whether
// a bucket exists. Assembly writes the merged set here whenever it rebuilds, so
// a bucket created through the API is visible to the admin object endpoints, the
// browser CORS policy, S3 admission, the reconciler and the web UI at the same
// moment it becomes reachable over S3.
//
// Held behind an atomic pointer and swapped whole, matching how the credential
// registry is published: readers on request goroutines never take a lock, and a
// reader mid-call keeps the consistent set it started with.
// -------------------------------------------------------------------------------

package provisioning

import (
	"strings"
	"sync/atomic"
)

// Declared is the merged bucket set, live. Registry assembly stores it at
// startup, after each reload, and after each provisioning change. The zero
// value is usable and reports nothing declared, which is what a process holds
// before its first assembly.
type Declared struct {
	set atomic.Pointer[declaredSet]
}

// declaredSet is one published bucket set, its names indexed so Contains,
// which S3 admission asks on every request, does not scan it.
type declaredSet struct {
	buckets []Bucket
	names   map[string]struct{}
}

// NewDeclared builds an empty holder.
func NewDeclared() *Declared {
	return &Declared{}
}

// Store publishes buckets as the declared set, replacing the one before it.
// The slice is kept, not copied, so the caller must not modify it after.
func (d *Declared) Store(buckets []Bucket) {
	names := make(map[string]struct{}, len(buckets))
	for i := range buckets {
		names[buckets[i].Name] = struct{}{}
	}
	d.set.Store(&declaredSet{buckets: buckets, names: names})
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
	s := d.load()
	if s == nil {
		return false
	}
	_, ok := s.names[name]
	return ok
}

// HasPrefix reports whether an object key names a declared bucket, which is the
// check that keeps an admin object call from addressing a namespace nothing
// declares. A key equal to a bucket name with no trailing slash names no
// object, so it does not match.
func (d *Declared) HasPrefix(key string) bool {
	buckets := d.Buckets()
	for i := range buckets {
		if strings.HasPrefix(key, buckets[i].Name+"/") {
			return true
		}
	}
	return false
}
