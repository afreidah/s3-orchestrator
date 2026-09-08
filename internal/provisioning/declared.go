// -------------------------------------------------------------------------------
// Provisioning - The Live Declared Bucket Set
//
// Author: Alex Freidah
//
// What every subsystem outside the auth path asks when it needs to know whether
// a bucket exists. Assembly writes the merged set here whenever it rebuilds, so
// a bucket created through the API is visible to the admin object endpoints, the
// browser CORS policy, the reconciler and the web UI at the same moment it
// becomes reachable over S3.
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

// Declared is the merged bucket set, live. The zero value is usable and reports
// nothing declared, which is what a process holds before its first assembly.
type Declared struct {
	buckets atomic.Pointer[[]Bucket]
}

// NewDeclared builds an empty holder.
func NewDeclared() *Declared {
	return &Declared{}
}

// Set replaces the declared set. Called by registry assembly, at startup, after
// each reload, and after each provisioning change.
func (d *Declared) Set(buckets []Bucket) {
	if d == nil {
		return
	}
	d.buckets.Store(&buckets)
}

// Buckets returns the declared set. The slice is the stored one and must not be
// modified; callers that need to keep it past the next swap copy it.
func (d *Declared) Buckets() []Bucket {
	if d == nil {
		return nil
	}
	if p := d.buckets.Load(); p != nil {
		return *p
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
	buckets := d.Buckets()
	for i := range buckets {
		if buckets[i].Name == name {
			return true
		}
	}
	return false
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
