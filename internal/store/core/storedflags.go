// -------------------------------------------------------------------------------
// Stored Form Flags - Which Layers a Copy's Bytes Carry
//
// Author: Alex Freidah
//
// One bit per fact about a copy's stored bytes: compressed, encrypted, hashed,
// and keyed when the row holds the key that opens its envelope. The store keeps
// the same set in object_locations.form, a column generated from the columns
// the bits summarize, so SQL filters on a mask while Go reads Form() from the
// same fields. A parity test in each engine pins the two definitions together.
// Bits are only ever added, never renumbered, since the column persists them.
// -------------------------------------------------------------------------------

package core

import (
	"encoding/json"

	"github.com/afreidah/s3-orchestrator/internal/util/bitset"
)

// StoredFlags is the set of layers a copy's stored bytes carry. It is signed to
// match the database integer column the store generates it into.
type StoredFlags int64

// The stored-form flags. Keyed without Encrypted does not occur; Encrypted
// without Keyed is an envelope nothing can open.
const (
	StoredCompressed StoredFlags = 1 << iota
	StoredEncrypted
	StoredHashed
	StoredKeyed
)

// StoredReadable is what an encrypted copy needs to be opened: the envelope and
// the key to it. StoredLayers is every flag that makes the stored bytes differ
// from the bytes the client wrote.
const (
	StoredReadable = StoredEncrypted | StoredKeyed
	StoredLayers   = StoredCompressed | StoredEncrypted
)

// storedFlagNames pairs each flag with its name, in the order a rendered set
// lists them.
var storedFlagNames = bitset.Vocabulary[StoredFlags]{
	{Bit: StoredCompressed, Name: "compressed"},
	{Bit: StoredEncrypted, Name: "encrypted"},
	{Bit: StoredHashed, Name: "hashed"},
	{Bit: StoredKeyed, Name: "keyed"},
}

// Has reports whether the set carries every flag in want.
func (f StoredFlags) Has(want StoredFlags) bool {
	return bitset.Has(f, want)
}

// Any reports whether the set carries at least one flag in mask.
func (f StoredFlags) Any(mask StoredFlags) bool {
	return bitset.Any(f, mask)
}

// Names lists the flags in the set by name.
func (f StoredFlags) Names() []string {
	return storedFlagNames.Names(f)
}

// String renders the set as its flag names joined by commas.
func (f StoredFlags) String() string {
	return storedFlagNames.Join(f)
}

// MarshalJSON renders the set as its flag names, so an API carrying it reads
// ["compressed","hashed"] rather than a bit number.
func (f StoredFlags) MarshalJSON() ([]byte, error) {
	return json.Marshal(f.Names())
}

// Unreadable reports whether the set describes an envelope with no key to open
// it, which is how import records encrypted bytes no surviving row can decrypt.
func (f StoredFlags) Unreadable() bool {
	return f.Has(StoredEncrypted) && !f.Has(StoredKeyed)
}

// Form returns the flags the stored form's own fields imply. It is the Go half
// of the definition object_locations.form generates, and the two must agree.
func (f *StoredForm) Form() StoredFlags {
	if f == nil {
		return 0
	}
	var set StoredFlags
	if f.CompressionAlgorithm != "" {
		set |= StoredCompressed
	}
	if f.Encrypted {
		set |= StoredEncrypted
	}
	if f.ContentHash != "" {
		set |= StoredHashed
	}
	if len(f.EncryptionKey) > 0 {
		set |= StoredKeyed
	}
	return set
}

// Recorded returns the form as a row stores it: the envelope fields only when
// the bytes are encrypted, and the encoding fields only when they are
// compressed. Both engines insert this, so a stray key never reads as keyed.
func (f *StoredForm) Recorded() StoredForm {
	r := StoredForm{ContentHash: f.ContentHash}
	if f.Encrypted {
		r.Encrypted = true
		r.EncryptionKey = f.EncryptionKey
		r.KeyID = f.KeyID
		r.PlaintextSize = f.PlaintextSize
	}
	if f.CompressionAlgorithm != "" {
		r.CompressionAlgorithm = f.CompressionAlgorithm
		r.CompressionLevel = f.CompressionLevel
		r.CompressionFormatVersion = f.CompressionFormatVersion
		r.LogicalSize = f.LogicalSize
	}
	return r
}
