// -------------------------------------------------------------------------------
// Backend Calls
//
// Author: Alex Freidah
//
// The backend calls a backend is serving right now, as a bit per call. Each
// call has its own circuit breaker, and its bit is clear while that breaker is
// open. A consumer asks whether a backend serves every call its purpose makes,
// through the required mask for that purpose, the same way a request asks
// whether a grant carries the permissions it needs. The masks live here so
// consumers that make the same calls share one definition.
//
// The set lives in memory per instance and is never stored, so bits may be
// renumbered.
// -------------------------------------------------------------------------------

package core

import (
	"encoding/json"

	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/util/bitset"
)

// BackendCalls is the set of backend calls a backend is serving. Bits are
// cleared when a call's breaker opens and set again when it closes, and the
// set is tested against a required mask.
type BackendCalls uint64

// The calls a circuit breaker guards, one bit each. Multipart has no calls of
// its own: a part is a PutObject to a temporary key.
const (
	CallPutObject BackendCalls = 1 << iota
	CallGetObject
	CallHeadObject
	CallDeleteObject
	CallDeleteObjects
	CallCopyObject
	CallListObjects
)

// AllCalls is every call, which is what a backend with no open breaker, or
// with no breaker at all, serves.
const AllCalls = CallPutObject | CallGetObject | CallHeadObject | CallDeleteObject |
	CallDeleteObjects | CallCopyObject | CallListObjects

// The required masks. RequiredForWrite is what a backend must serve to be
// given new bytes - client writes, multipart parts, replicas, rebalance moves
// and drain destinations. It includes GetObject because a copy nobody can read
// back is not worth writing: it would leave the object unreadable and give the
// replicator no source to copy from. RequiredForRead is what makes a copy on it
// readable, for reads and replication sources. RequiredForProbe is what
// checking whether a key landed on it needs.
const (
	RequiredForWrite = CallPutObject | CallGetObject
	RequiredForRead  = CallGetObject
	RequiredForProbe = CallHeadObject
)

// backendCallNames pairs each call with its name, which is the operation's
// name, in the order a rendered set lists them.
var backendCallNames = bitset.Vocabulary[BackendCalls]{
	{Bit: CallPutObject, Name: s3op.PutObject.String()},
	{Bit: CallGetObject, Name: s3op.GetObject.String()},
	{Bit: CallHeadObject, Name: s3op.HeadObject.String()},
	{Bit: CallDeleteObject, Name: s3op.DeleteObject.String()},
	{Bit: CallDeleteObjects, Name: s3op.DeleteObjects.String()},
	{Bit: CallCopyObject, Name: s3op.CopyObject.String()},
	{Bit: CallListObjects, Name: s3op.ListObjects.String()},
}

// Has reports whether the set serves every call in required. An empty
// required mask is satisfied by any set.
func (c BackendCalls) Has(required BackendCalls) bool {
	return bitset.Has(c, required)
}

// Names lists the calls in the set by name.
func (c BackendCalls) Names() []string {
	return backendCallNames.Names(c)
}

// String renders the set as its call names joined by commas.
func (c BackendCalls) String() string {
	return backendCallNames.Join(c)
}

// MarshalJSON renders the set as its call names, so an API carrying it reads
// ["GetObject"] rather than a bit number.
func (c BackendCalls) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.Names())
}

// CallFor returns the bit for the backend call op names, and false for an
// operation no breaker guards.
func CallFor(op s3op.Operation) (BackendCalls, bool) {
	return backendCallNames.Lookup(op.String())
}
