// -------------------------------------------------------------------------------
// Provisioning - Permission Set Tests
//
// Author: Alex Freidah
//
// The bit set a grant carries, and the text form it is stored and rendered as.
// The round trip matters most: a set that parses to less than it was written as
// refuses a caller the operator authorized, and one that parses to more grants
// access nobody wrote down.
// -------------------------------------------------------------------------------

package core

import "testing"

// TestPermissionSet_BitsAreDistinct pins that every permission occupies its own
// bit. Two sharing one would make granting either grant both.
func TestPermissionSet_BitsAreDistinct(t *testing.T) {
	t.Parallel()

	seen := map[PermissionSet]string{}
	for _, p := range []struct {
		bit  PermissionSet
		name string
	}{
		{PermListBuckets, "list-buckets"},
		{PermList, "list"},
		{PermRead, "read"},
		{PermWrite, "write"},
		{PermDelete, "delete"},
		{PermTags, "tags"},
	} {
		if p.bit == 0 {
			t.Errorf("%s has no bit", p.name)
		}
		if p.bit&(p.bit-1) != 0 {
			t.Errorf("%s = %d, which is not a single bit", p.name, p.bit)
		}
		if prior, ok := seen[p.bit]; ok {
			t.Errorf("%s shares a bit with %s", p.name, prior)
		}
		seen[p.bit] = p.name
		if !PermAll.Has(p.bit) {
			t.Errorf("PermAll does not carry %s", p.name)
		}
	}
}

// TestPermissionSet_Has covers the comparison every request runs, including the
// multi-bit case a copy needs.
func TestPermissionSet_Has(t *testing.T) {
	t.Parallel()

	readWrite := PermRead | PermWrite
	for _, tc := range []struct {
		name string
		held PermissionSet
		want PermissionSet
		ok   bool
	}{
		{"single bit held", readWrite, PermRead, true},
		{"single bit absent", readWrite, PermDelete, false},
		{"both bits held", readWrite, readWrite, true},
		{"one of two absent", readWrite, PermRead | PermDelete, false},
		{"nothing required", readWrite, 0, true},
		{"nothing required of nothing", 0, 0, true},
		{"something required of nothing", 0, PermRead, false},
		{"all carries each", PermAll, PermTags, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.held.Has(tc.want); got != tc.ok {
				t.Errorf("Has = %t, want %t", got, tc.ok)
			}
		})
	}
}

// TestPermissions_RoundTrip verifies a set renders to text that parses back to
// the same set, which is what makes the column a faithful record of the grant.
func TestPermissions_RoundTrip(t *testing.T) {
	t.Parallel()

	for _, set := range []PermissionSet{
		PermRead,
		PermRead | PermWrite,
		PermListBuckets | PermList | PermRead,
		PermAll,
		PermTags | PermDelete,
	} {
		got, err := ParsePermissions(set.String())
		if err != nil {
			t.Fatalf("ParsePermissions(%q): %v", set.String(), err)
		}
		if got != set {
			t.Errorf("round trip of %q gave %q", set.String(), got.String())
		}
	}
}

// TestParsePermissions verifies the forms a stored value or an API request may
// arrive in.
func TestParsePermissions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want PermissionSet
	}{
		{"empty is every permission", "", PermAll},
		{"whitespace is every permission", "   ", PermAll},
		{"all is every permission", "all", PermAll},
		{"single", "read", PermRead},
		{"several", "read,write", PermRead | PermWrite},
		{"spaces are trimmed", " read , write ", PermRead | PermWrite},
		{"case is ignored", "READ,Write", PermRead | PermWrite},
		{"repeats collapse", "read,read", PermRead},
		{"empty fields are skipped", "read,,write", PermRead | PermWrite},
		{"hyphenated name", "list-buckets", PermListBuckets},
		{"all alongside a name", "all,read", PermAll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParsePermissions(tc.in)
			if err != nil {
				t.Fatalf("ParsePermissions(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParsePermissions(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestParsePermissions_RejectsUnknown verifies an unrecognised name fails
// rather than being dropped. A set that silently parses to less than it says
// would refuse a caller the operator believes they authorized.
func TestParsePermissions_RejectsUnknown(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"admin", "read,admin", "readwrite", "list_buckets"} {
		if _, err := ParsePermissions(in); err == nil {
			t.Errorf("ParsePermissions(%q) accepted an unknown permission", in)
		}
	}
}

// TestPermissionSet_String verifies the rendered order is fixed, so two equal
// sets always store and display identically.
func TestPermissionSet_String(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		set  PermissionSet
		want string
	}{
		{0, ""},
		{PermRead, "read"},
		{PermWrite | PermRead, "read,write"},
		{PermDelete | PermList, "list,delete"},
		{PermAll, "all"},
	} {
		if got := tc.set.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

// TestPermissionSet_Names verifies the individual form an API response renders,
// which stays a list even for the full set the column shortens to "all".
func TestPermissionSet_Names(t *testing.T) {
	t.Parallel()

	if got := len(PermAll.Names()); got != 6 {
		t.Errorf("PermAll.Names() has %d entries, want 6", got)
	}
	got := (PermRead | PermDelete).Names()
	if len(got) != 2 || got[0] != "read" || got[1] != "delete" {
		t.Errorf("Names() = %v, want [read delete]", got)
	}
	if n := len(PermissionSet(0).Names()); n != 0 {
		t.Errorf("an empty set names %d permissions, want 0", n)
	}
}
