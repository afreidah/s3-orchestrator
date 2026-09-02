// -------------------------------------------------------------------------------
// S3 Operation Vocabulary
//
// Author: Alex Freidah
//
// The closed set of operations the orchestrator issues against a storage
// backend, as one type rather than a string literal per call site. Providers
// bill these differently - an upload and a delete are not the same charge -
// so config, usage accounting and the operation metric all have to agree on
// what an operation is called. A leaf package with no app dependencies, so
// config, the counter layer and the proxy can all name an operation without
// importing one another.
// -------------------------------------------------------------------------------

package s3op

// Operation names one backend call. The set is closed: a backend call the
// orchestrator can make is one of these, and config validation rejects
// anything else so an operator cannot budget for an operation that will never
// be charged.
type Operation string

// The operations the orchestrator issues against a backend.
const (
	PutObject               Operation = "PutObject"
	GetObject               Operation = "GetObject"
	HeadObject              Operation = "HeadObject"
	DeleteObject            Operation = "DeleteObject"
	DeleteObjects           Operation = "DeleteObjects"
	CopyObject              Operation = "CopyObject"
	ListObjects             Operation = "ListObjects"
	ListObjectsV2           Operation = "ListObjectsV2"
	CreateMultipartUpload   Operation = "CreateMultipartUpload"
	UploadPart              Operation = "UploadPart"
	CompleteMultipartUpload Operation = "CompleteMultipartUpload"
	AbortMultipartUpload    Operation = "AbortMultipartUpload"
	GetParts                Operation = "GetParts"
)

// Wildcard matches every operation not listed as unmetered. Written in a
// pool's operation list as "*".
const Wildcard = "*"

// all is the authoritative set, ordered as declared above so generated
// documentation and error messages list operations predictably.
var all = []Operation{
	PutObject,
	GetObject,
	HeadObject,
	DeleteObject,
	DeleteObjects,
	CopyObject,
	ListObjects,
	ListObjectsV2,
	CreateMultipartUpload,
	UploadPart,
	CompleteMultipartUpload,
	AbortMultipartUpload,
	GetParts,
}

// All returns every known operation. The caller receives a copy, so a
// config validator or a pool compiler can sort or filter it freely.
func All() []Operation {
	out := make([]Operation, len(all))
	copy(out, all)
	return out
}

// Known reports whether name is an operation the orchestrator issues.
// Config validation uses this to reject a budget written against an
// operation that does not exist, which would otherwise silently never
// be charged.
func Known(name string) bool {
	for _, op := range all {
		if string(op) == name {
			return true
		}
	}
	return false
}

// String renders the operation for metric labels and log fields.
func (o Operation) String() string { return string(o) }
