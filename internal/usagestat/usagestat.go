// -------------------------------------------------------------------------------
// Usage Stat - The Request and Byte Triple
//
// Author: Alex Freidah
//
// The requests made of a backend and the bytes it sent and received. Counted
// by the usage counters, summed into the stored usage rows, and tallied per
// pass for audit, all as this one shape. A leaf package so each of those can
// use it without importing the others.
// -------------------------------------------------------------------------------

package usagestat

// Stat is what a backend was charged: the requests made of it and the bytes it
// sent and received.
type Stat struct {
	APIRequests  int64
	EgressBytes  int64
	IngressBytes int64
}

// Plus returns the sum of two stats.
func (s Stat) Plus(o Stat) Stat {
	return Stat{
		APIRequests:  s.APIRequests + o.APIRequests,
		EgressBytes:  s.EgressBytes + o.EgressBytes,
		IngressBytes: s.IngressBytes + o.IngressBytes,
	}
}
