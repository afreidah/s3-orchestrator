// -------------------------------------------------------------------------------
// Provisioning - Grant Permissions
//
// Author: Alex Freidah
//
// The access a grant carries, as a bit per permission. A request is authorized
// by testing the permissions its operation needs against the set the caller's
// grant holds, which is one comparison however many bits either side names.
//
// The set is deliberately intent-shaped rather than one bit per S3 call: the
// same intents an operator recognises from IAM and from Azure's blob actions,
// so a grant written here means what it looks like it means on either. A
// permission added later takes the next power of two and nothing renumbers.
//
// The stored and rendered form is a comma-separated list rather than the
// integer, so a grant row read in psql or returned by the API says what it
// allows. Conversion happens where a row is mapped, and nothing above the store
// sees the text.
// -------------------------------------------------------------------------------

package core

import (
	"fmt"
	"strings"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// PermissionSet is the access a grant carries. Bits are combined with OR when
// the set is built and tested with AND when a request is authorized.
type PermissionSet uint16

// The permissions a grant may carry, one per operator-facing intent.
//
// ListBuckets and List are separate because they answer different questions: a
// client may be entitled to know a bucket exists without being entitled to
// enumerate what is in it, which is the same split AWS draws between
// s3:ListAllMyBuckets and s3:ListBucket. Read is the object body alone.
//
// Tags covers reading and writing them in one intent, matching how object
// metadata is granted rather than splitting a small surface three ways.
const (
	PermListBuckets PermissionSet = 1 << iota
	PermList
	PermRead
	PermWrite
	PermDelete
	PermTags
)

// PermAll is every permission this server implements. It is also what a grant
// recording none carries, which is every grant written before permissions
// existed: narrowing those on upgrade would refuse clients that were working.
const PermAll = PermListBuckets | PermList | PermRead | PermWrite | PermDelete | PermTags

// permAllName is the shorthand a caller writes instead of naming every
// permission, and what PermAll renders as.
const permAllName = "all"

// permissionNames pairs each bit with its stored name, in the order a rendered
// set lists them, so two equal sets always render identically.
var permissionNames = []struct {
	bit  PermissionSet
	name string
}{
	{PermListBuckets, "list-buckets"},
	{PermList, "list"},
	{PermRead, "read"},
	{PermWrite, "write"},
	{PermDelete, "delete"},
	{PermTags, "tags"},
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Has reports whether this set carries every permission in want. Taking a set
// rather than a single bit is what lets an operation needing more than one -
// a copy reads its source and writes its destination - be authorized by one
// comparison.
//
// An empty want is satisfied by any set, which is what an operation needing no
// permission asks for.
func (p PermissionSet) Has(want PermissionSet) bool {
	return p&want == want
}

// String renders the set as the stored form: a comma-separated list in a fixed
// order, or "all" for the full set. An empty set renders empty, which is what a
// grant carrying nothing is.
func (p PermissionSet) String() string {
	if p == PermAll {
		return permAllName
	}
	var out []string
	for _, entry := range permissionNames {
		if p&entry.bit != 0 {
			out = append(out, entry.name)
		}
	}
	return strings.Join(out, ",")
}

// Names lists the permissions in the set individually, which is what an API
// response renders rather than the joined string a column holds.
func (p PermissionSet) Names() []string {
	out := make([]string, 0, len(permissionNames))
	for _, entry := range permissionNames {
		if p&entry.bit != 0 {
			out = append(out, entry.name)
		}
	}
	return out
}

// ParsePermissions reads the stored form. An empty string is PermAll, because a
// grant that records no permissions predates them and carried full access.
//
// An unrecognised name is an error rather than a bit quietly dropped. A set
// that parses to less than it says would refuse a caller an operator believes
// they authorized, and one that defaults to PermAll on a value nothing
// recognises would grant access nobody wrote down.
func ParsePermissions(s string) (PermissionSet, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return PermAll, nil
	}

	var out PermissionSet
	for _, field := range strings.Split(s, ",") {
		name := strings.ToLower(strings.TrimSpace(field))
		if name == "" {
			continue
		}
		if name == permAllName {
			out |= PermAll
			continue
		}
		bit, ok := permissionBit(name)
		if !ok {
			return 0, fmt.Errorf("unknown permission %q, want one of %s or %q",
				name, strings.Join(PermAll.Names(), ", "), permAllName)
		}
		out |= bit
	}
	if out == 0 {
		return PermAll, nil
	}
	return out, nil
}

// permissionBit looks up the bit a stored name selects.
func permissionBit(name string) (PermissionSet, bool) {
	for _, entry := range permissionNames {
		if entry.name == name {
			return entry.bit, true
		}
	}
	return 0, false
}
