// -------------------------------------------------------------------------------
// Stored Form Flags - Tests
//
// Author: Alex Freidah
//
// The flag vocabulary, the unreadable judgement, and the set Form derives from
// a stored form's fields. Form is the Go half of the definition the engines
// generate into object_locations.form, so each field's bit is pinned here.
// -------------------------------------------------------------------------------

package core

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// TestStoredFlags_BitsAreDistinct pins that every flag occupies its own bit.
func TestStoredFlags_BitsAreDistinct(t *testing.T) {
	t.Parallel()
	if err := storedFlagNames.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestStoredFlags_Render covers the names a set lists, in vocabulary order,
// and the comma-joined form.
func TestStoredFlags_Render(t *testing.T) {
	t.Parallel()
	all := StoredCompressed | StoredEncrypted | StoredHashed | StoredKeyed
	if got := all.Names(); !slices.Equal(got, []string{"compressed", "encrypted", "hashed", "keyed"}) {
		t.Errorf("Names = %v", got)
	}
	if got := (StoredHashed | StoredCompressed).String(); got != "compressed,hashed" {
		t.Errorf("String = %q", got)
	}
	if got := StoredFlags(0).String(); got != "" {
		t.Errorf("empty String = %q, want empty", got)
	}
}

// TestStoredFlags_MarshalJSON pins the JSON form to the flag names.
func TestStoredFlags_MarshalJSON(t *testing.T) {
	t.Parallel()
	for flags, want := range map[StoredFlags]string{
		0:                               `[]`,
		StoredHashed:                    `["hashed"]`,
		StoredCompressed | StoredHashed: `["compressed","hashed"]`,
		StoredReadable:                  `["encrypted","keyed"]`,
	} {
		got, err := json.Marshal(flags)
		if err != nil || string(got) != want {
			t.Errorf("json.Marshal(%d) = %s, %v; want %s", int64(flags), got, err, want)
		}
	}
}

// TestStoredFlags_HasAndAny verifies Has needs every flag of the mask and Any
// needs one.
func TestStoredFlags_HasAndAny(t *testing.T) {
	t.Parallel()
	set := StoredEncrypted | StoredHashed
	if set.Has(StoredReadable) {
		t.Error("Has(Readable) without the key")
	}
	if !set.Any(StoredReadable) {
		t.Error("Any(Readable) false with the envelope present")
	}
	if set.Any(StoredCompressed | StoredKeyed) {
		t.Error("Any matched a mask the set shares nothing with")
	}
}

// TestStoredFlags_Unreadable verifies only an envelope with no key is
// unreadable.
func TestStoredFlags_Unreadable(t *testing.T) {
	t.Parallel()
	for flags, want := range map[StoredFlags]bool{
		0:                                 false,
		StoredHashed:                      false,
		StoredKeyed:                       false,
		StoredEncrypted:                   true,
		StoredEncrypted | StoredHashed:    true,
		StoredReadable:                    false,
		StoredReadable | StoredCompressed: false,
	} {
		if got := flags.Unreadable(); got != want {
			t.Errorf("%s.Unreadable() = %v, want %v", flags, got, want)
		}
	}
}

// TestStoredForm_Form verifies each field sets its own bit and nothing else,
// and that a nil form has no flags.
func TestStoredForm_Form(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		form *StoredForm
		want StoredFlags
	}{
		{"nil", nil, 0},
		{"plain", &StoredForm{}, 0},
		{"compressed", &StoredForm{CompressionAlgorithm: "zstd"}, StoredCompressed},
		{"compression level alone", &StoredForm{CompressionLevel: "default", LogicalSize: 9}, 0},
		{"encrypted", &StoredForm{Encrypted: true}, StoredEncrypted},
		{"key id alone", &StoredForm{KeyID: "kid", PlaintextSize: 9}, 0},
		{"hashed", &StoredForm{ContentHash: "h"}, StoredHashed},
		{"keyed", &StoredForm{EncryptionKey: []byte("dek")}, StoredKeyed},
		{"empty key", &StoredForm{Encrypted: true, EncryptionKey: []byte{}}, StoredEncrypted},
		{"every layer", &StoredForm{
			CompressionAlgorithm: "zstd", Encrypted: true, ContentHash: "h", EncryptionKey: []byte("dek"),
		}, StoredCompressed | StoredEncrypted | StoredHashed | StoredKeyed},
	}
	for _, tc := range cases {
		if got := tc.form.Form(); got != tc.want {
			t.Errorf("%s: Form() = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestStoredForm_Recorded checks that a row keeps the envelope and encoding
// fields only alongside the layer they describe, and always keeps the hash.
func TestStoredForm_Recorded(t *testing.T) {
	stray := &StoredForm{
		EncryptionKey: []byte("dek"), KeyID: "k1", PlaintextSize: 10, ContentHash: "h",
		CompressionLevel: "fast", CompressionFormatVersion: 1, LogicalSize: 50,
	}
	if got := stray.Recorded(); !reflect.DeepEqual(got, StoredForm{ContentHash: "h"}) {
		t.Errorf("stray fields Recorded() = %+v, want only the hash", got)
	}

	full := &StoredForm{
		Encrypted: true, EncryptionKey: []byte("dek"), KeyID: "k1", PlaintextSize: 10, ContentHash: "h",
		CompressionAlgorithm: "zstd", CompressionLevel: "fast", CompressionFormatVersion: 1, LogicalSize: 50,
	}
	if got := full.Recorded(); !reflect.DeepEqual(got, *full) {
		t.Errorf("full form Recorded() = %+v, want it unchanged", got)
	}
}
