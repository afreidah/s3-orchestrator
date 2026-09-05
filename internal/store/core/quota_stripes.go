// -------------------------------------------------------------------------------
// Quota Stripes - Contention-Free Byte Counters
//
// Author: Alex Freidah
//
// A backend's stored byte total lives across several rows rather than one, so
// concurrent writes charging the same backend take different row locks instead
// of queueing behind a single one. The total is the sum across a backend's
// stripes; an individual stripe is signed and carries no meaning alone.
//
// A key always maps to the same stripe, so an object's charge and the credit
// that later reverses it meet on one row rather than skewing two.
// -------------------------------------------------------------------------------

package core

import "hash/fnv"

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// QuotaStripeCount is how many rows a backend's byte counter is split across.
// Safe to change between releases: a larger value leaves the new stripes empty
// until writes reach them, a smaller one leaves the surplus read-only, and the
// sum stays correct throughout because no stripe is meaningful on its own.
const QuotaStripeCount = 16

// -------------------------------------------------------------------------
// SELECTION
// -------------------------------------------------------------------------

// StripeFor maps an object key to the stripe that holds its bytes.
//
// Hashing the key rather than choosing at random is what keeps a delete on the
// same row as the write it reverses. Random selection would still sum
// correctly, but every object would leave one stripe high and another low, and
// a stripe's value would carry no relationship to anything stored.
func StripeFor(key string) int16 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key)) // hash.Hash never reports an error
	return int16(h.Sum32() % QuotaStripeCount)
}
