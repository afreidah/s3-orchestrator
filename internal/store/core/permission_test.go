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
		got, err := ParsePermissions(ResourceBucket, set.String())
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
		kind ResourceKind
		in   string
		want PermissionSet
	}{
		{"empty is every permission", ResourceBucket, "", PermAll},
		{"whitespace is every permission", ResourceBucket, "   ", PermAll},
		{"all is every permission", ResourceBucket, "all", PermAll},
		{"single", ResourceBucket, "read", PermRead},
		{"several", ResourceBucket, "read,write", PermRead | PermWrite},
		{"spaces are trimmed", ResourceBucket, " read , write ", PermRead | PermWrite},
		{"case is ignored", ResourceBucket, "READ,Write", PermRead | PermWrite},
		{"repeats collapse", ResourceBucket, "read,read", PermRead},
		{"empty fields are skipped", ResourceBucket, "read,,write", PermRead | PermWrite},
		{"hyphenated name", ResourceBucket, "list-buckets", PermListBuckets},
		{"all alongside a name", ResourceBucket, "all,read", PermAll},
		{"admin-all is every admin permission", ResourceBackend, "admin-all", PermAdminAll},
		{"admin names", ResourceOrchestrator, "admin-read,admin-logs", PermAdminRead | PermAdminLogs},
		{"empty on a backend is nothing", ResourceBackend, "", 0},
		{"empty on the instance is nothing", ResourceOrchestrator, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParsePermissions(tc.kind, tc.in)
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
		if _, err := ParsePermissions(ResourceBucket, in); err == nil {
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
		{PermAdminAll, "admin-all"},
		{PermAdminDrain, "admin-drain"},
		{PermAdminRead | PermAdminProvision, "admin-read,admin-provision"},
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

// TestValidatePermissions verifies a grant carrying permissions that mean
// nothing on the resource it names is refused. The two vocabularies share a
// type, so this is what keeps them from being mixed in one grant: a bucket
// cannot be drained, and the instance holds no objects.
func TestValidatePermissions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		kind    ResourceKind
		perms   PermissionSet
		wantErr bool
	}{
		{"data-plane on a bucket", ResourceBucket, PermRead | PermWrite, false},
		{"every data-plane on a bucket", ResourceBucket, PermAll, false},
		{"admin on a bucket", ResourceBucket, PermAdminDrain, true},
		{"admin mixed into a bucket grant", ResourceBucket, PermRead | PermAdminDrain, true},
		{"admin on a backend", ResourceBackend, PermAdminConvert, false},
		{"admin on the instance", ResourceOrchestrator, PermAdminProvision, false},
		{"data-plane on a backend", ResourceBackend, PermRead, true},
		{"data-plane on the instance", ResourceOrchestrator, PermList, true},
		{"nothing is valid anywhere", ResourceBackend, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePermissions(tc.kind, tc.perms)
			if tc.wantErr && err == nil {
				t.Errorf("ValidatePermissions(%s, %q) accepted a set the resource has no meaning for", tc.kind, tc.perms)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidatePermissions(%s, %q): %v", tc.kind, tc.perms, err)
			}
		})
	}
}

// TestPermissionBits_DoNotOverlap verifies the two vocabularies occupy
// disjoint bits. They share one type, so an overlap would make a bucket grant
// silently authorize a control-plane operation.
func TestPermissionBits_DoNotOverlap(t *testing.T) {
	t.Parallel()

	if PermAll&PermAdminAll != 0 {
		t.Errorf("the data-plane and admin sets share bits: %q", PermAll&PermAdminAll)
	}
}

// TestParseResourceKind covers the spellings a stored row or a submitted
// request may carry, including the retired one.
func TestParseResourceKind(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want ResourceKind
	}{
		{"empty is the bucket", "", ResourceBucket},
		{"bucket", "bucket", ResourceBucket},
		{"backend", "backend", ResourceBackend},
		{"orchestrator", "orchestrator", ResourceOrchestrator},
		{"the retired instance spelling", "instance", ResourceOrchestrator},
		{"an unknown kind is left alone to be refused later", "nonsense", ResourceKind("nonsense")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseResourceKind(tc.in); got != tc.want {
				t.Errorf("ParseResourceKind(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestResourceString_OrchestratorCarriesNoName verifies the orchestrator
// renders bare while the other kinds render with the resource they name, so a
// grant listing and an audit entry read alike.
func TestResourceString_OrchestratorCarriesNoName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   Resource
		want string
	}{
		{Resource{Kind: ResourceOrchestrator}, "orchestrator"},
		{Resource{Kind: ResourceBucket, Name: "photos"}, "bucket:photos"},
		{Resource{Kind: ResourceBackend, Name: "*"}, "backend:*"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Resource.String() = %q, want %q", got, tc.want)
		}
	}
}
