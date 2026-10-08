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
//
// Sixty-four bits for sixteen permissions is headroom: a new one takes the next
// bit and nothing renumbers.
type PermissionSet uint64

// The permissions a grant may carry, one per operator-facing intent.
// ListBuckets and List mirror the AWS split between s3:ListAllMyBuckets and
// s3:ListBucket. Read covers an object's bytes and its tags; Tags is the right
// to change them.
const (
	PermListBuckets PermissionSet = 1 << iota
	PermList
	PermRead
	PermWrite
	PermDelete
	PermTags
)

// The permissions a grant on a backend or on the orchestrator may carry. They
// continue past the data-plane bits so no set of bucket permissions can add up
// to an administrative one; ValidatePermissions checks which are valid where.
// Bits are stored by name, so they may be renumbered without touching rows.
// Provisioning stands alone because it can mint any other permission.
const (
	PermAdminRead PermissionSet = 1 << (6 + iota)
	PermAdminLogs
	PermAdminMaintain
	PermAdminConvert
	PermAdminKeys
	PermAdminCache
	PermAdminDrain
	PermAdminDecommission
	PermAdminConfig
	PermAdminProvision
)

// PermAll is every data-plane permission, and what a grant recording none
// carries - which is every grant written before permissions existed, so it
// cannot be widened to include the admin bits without granting them to rows
// nobody wrote them on.
const PermAll = PermListBuckets | PermList | PermRead | PermWrite | PermDelete | PermTags

// PermAdminAll is every admin permission. An admin grant recording none carries
// nothing rather than everything: no row predates this vocabulary, so there is
// no existing meaning to preserve.
const PermAdminAll = PermAdminRead | PermAdminLogs | PermAdminMaintain | PermAdminConvert |
	PermAdminKeys | PermAdminCache | PermAdminDrain | PermAdminDecommission |
	PermAdminConfig | PermAdminProvision

// The shorthands a caller writes instead of naming every permission in a set.
const (
	permAllName      = "all"
	permAdminAllName = "admin-all"
)

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
	{PermAdminRead, "admin-read"},
	{PermAdminLogs, "admin-logs"},
	{PermAdminMaintain, "admin-maintain"},
	{PermAdminConvert, "admin-convert"},
	{PermAdminKeys, "admin-keys"},
	{PermAdminCache, "admin-cache"},
	{PermAdminDrain, "admin-drain"},
	{PermAdminDecommission, "admin-decommission"},
	{PermAdminConfig, "admin-config"},
	{PermAdminProvision, "admin-provision"},
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Has reports whether this set carries every permission in want. An empty want
// is satisfied by any set.
func (p PermissionSet) Has(want PermissionSet) bool {
	return p&want == want
}

// String renders the set as the stored form: a comma-separated list in a fixed
// order, or "all" for the full set. An empty set renders empty, which is what a
// grant carrying nothing is.
func (p PermissionSet) String() string {
	switch p {
	case PermAll:
		return permAllName
	case PermAdminAll:
		return permAdminAllName
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

// ParsePermissions reads the stored form for a grant on the given resource. An
// empty value is PermAll on a bucket grant, since such rows predate
// permissions, and nothing on any other kind. An unrecognised name is an error.
func ParsePermissions(kind ResourceKind, s string) (PermissionSet, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return emptyPermissions(kind), nil
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
		if name == permAdminAllName {
			out |= PermAdminAll
			continue
		}
		bit, ok := permissionBit(name)
		if !ok {
			return 0, fmt.Errorf("unknown permission %q, want one of %s, %q or %q",
				name, strings.Join(allPermissionNames(), ", "), permAllName, permAdminAllName)
		}
		out |= bit
	}
	if out == 0 {
		return emptyPermissions(kind), nil
	}
	return out, nil
}

// emptyPermissions is what a grant recording nothing carries.
func emptyPermissions(kind ResourceKind) PermissionSet {
	if kind == ResourceBucket {
		return PermAll
	}
	return 0
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

// allPermissionNames lists every name a grant may carry, for the error a bad
// one produces.
func allPermissionNames() []string {
	out := make([]string, 0, len(permissionNames))
	for _, entry := range permissionNames {
		out = append(out, entry.name)
	}
	return out
}

// ValidOn reports the permissions a grant on the given resource may carry:
// the data-plane set for buckets and the admin set for everything else.
func ValidOn(kind ResourceKind) PermissionSet {
	if kind == ResourceBucket {
		return PermAll
	}
	return PermAdminAll
}

// ValidatePermissions refuses permissions that mean nothing on the resource a
// grant names, so an operator is told at once rather than finding the grant
// authorizes nothing later.
func ValidatePermissions(kind ResourceKind, p PermissionSet) error {
	stray := p &^ ValidOn(kind)
	if stray == 0 {
		return nil
	}
	return fmt.Errorf("permissions %s are not valid on a %s grant, want one of %s",
		stray, kind, strings.Join(ValidOn(kind).Names(), ", "))
}
