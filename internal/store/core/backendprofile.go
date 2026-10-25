// -------------------------------------------------------------------------------
// Backend Profile
//
// Author: Alex Freidah
//
// The fixed facts about a backend that its configuration declares and a reload
// does not change: where it is, which bucket holds its copies, and the largest
// object it accepts. Write placement reads the size cap from here, and the
// status surfaces report the rest, so both read one description of the backend.
// -------------------------------------------------------------------------------

package core

// BackendProfile describes one backend as configured. MaxObjectSize is zero
// when the backend takes an object of any size.
type BackendProfile struct {
	Endpoint      string
	Region        string
	Bucket        string
	MaxObjectSize int64
}

// Accepts reports whether the backend takes an object of size bytes.
func (p BackendProfile) Accepts(size int64) bool {
	return p.MaxObjectSize <= 0 || size <= p.MaxObjectSize
}
