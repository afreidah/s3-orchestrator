// -------------------------------------------------------------------------------
// bitset - Named Bits of an Integer Flag Set
//
// Author: Alex Freidah
//
// The part of a bit-flag type that does not depend on what the bits mean: the
// table pairing each bit with its name, in the order a rendered set lists them,
// rendering a set as names, looking a name up, checking that every name has a
// bit of its own, and the tests a set is compared to a mask with. A flag type
// declares its bits and one Vocabulary, renders through it, and tests through
// Has, Any and Within.
// -------------------------------------------------------------------------------

package bitset

import (
	"fmt"
	"math/bits"
	"strings"
)

// Flags is the integer kinds a flag type may be built on: unsigned for a set
// only Go reads, signed for one stored in a database integer column.
type Flags interface {
	~uint64 | ~int64
}

// Bit pairs one bit of a flag type with its name.
type Bit[T Flags] struct {
	Bit  T
	Name string
}

// Vocabulary is every named bit of a flag type, in the order a rendered set
// lists them, so two equal sets always render identically.
type Vocabulary[T Flags] []Bit[T]

// Has reports whether set carries every bit in want. An empty want is
// satisfied by any set.
func Has[T Flags](set, want T) bool {
	return (set & want) == want
}

// Any reports whether set carries at least one bit in mask.
func Any[T Flags](set, mask T) bool {
	return (set & mask) != 0
}

// Within reports whether every bit set carries is one allowed permits. An
// empty set is within any mask.
func Within[T Flags](set, allowed T) bool {
	return (set &^ allowed) == 0
}

// Lookup returns the bit name selects, and false when no entry carries it.
func (v Vocabulary[T]) Lookup(name string) (T, bool) {
	for _, b := range v {
		if b.Name == name {
			return b.Bit, true
		}
	}
	return 0, false
}

// AllNames lists every name in the vocabulary, in vocabulary order.
func (v Vocabulary[T]) AllNames() []string {
	out := make([]string, 0, len(v))
	for _, b := range v {
		out = append(out, b.Name)
	}
	return out
}

// Names lists the names of the bits set carries, in vocabulary order.
func (v Vocabulary[T]) Names(set T) []string {
	out := make([]string, 0, len(v))
	for _, b := range v {
		if (set & b.Bit) != 0 {
			out = append(out, b.Name)
		}
	}
	return out
}

// Join renders set as its names joined by commas, in vocabulary order. An
// empty set renders empty.
func (v Vocabulary[T]) Join(set T) string {
	return strings.Join(v.Names(set), ",")
}

// Validate reports the first entry that is not a single bit, shares its bit
// with an earlier entry, or repeats an earlier name. Two names on one bit
// would make setting either set both.
func (v Vocabulary[T]) Validate() error {
	seen := make(map[T]string, len(v))
	names := make(map[string]bool, len(v))
	for _, b := range v {
		if b.Bit <= 0 || bits.OnesCount64(uint64(b.Bit)) != 1 {
			return fmt.Errorf("%s = %d, which is not a single bit", b.Name, b.Bit)
		}
		if prior, ok := seen[b.Bit]; ok {
			return fmt.Errorf("%s shares a bit with %s", b.Name, prior)
		}
		if names[b.Name] {
			return fmt.Errorf("name %s appears twice", b.Name)
		}
		seen[b.Bit] = b.Name
		names[b.Name] = true
	}
	return nil
}
