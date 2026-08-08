// -------------------------------------------------------------------------------
// Core Domain Types - Engine-Agnostic Store Types
//
// Author: Alex Freidah
//
// Canonical domain types shared by the Postgres and SQLite store engines. Engine
// adapters translate between sqlc-generated row structs and these types so that
// engine-agnostic business logic in this package never touches pgtype.* or
// sql.Null* values directly.
// -------------------------------------------------------------------------------

package core

import "time"

// -------------------------------------------------------------------------
// OBJECT METADATA
// -------------------------------------------------------------------------

// EncryptionMeta carries envelope-encryption metadata stored alongside an
// object location. The zero value represents an unencrypted object.
type EncryptionMeta struct {
	Encrypted     bool
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
	ContentHash   string

	// Unmanaged marks an object that exists on the backend but outside every
	// configured virtual bucket prefix: real bytes the orchestrator did not
	// write and does not act on. It is stored as the negated `managed` column,
	// so the zero value means managed and a construction site that omits it
	// cannot accidentally produce a row the workers ignore.
	Unmanaged bool
}

// ObjectLocation records that a backend currently holds a copy of a key,
// along with the size and any encryption or integrity metadata.
type ObjectLocation struct {
	ObjectKey     string
	BackendName   string
	SizeBytes     int64
	CreatedAt     time.Time
	Encrypted     bool
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
	ContentHash   string

	// Unmanaged marks an object that exists on the backend but outside every
	// configured virtual bucket prefix: real bytes the orchestrator did not
	// write and does not act on. It is stored as the negated `managed` column,
	// so the zero value means managed and a construction site that omits it
	// cannot accidentally produce a row the workers ignore.
	Unmanaged bool
}

// ExistingCopy is the projection of an object_locations row that promotion
// and overwrite logic needs from a SELECT-for-update read.
type ExistingCopy struct {
	BackendName string
	SizeBytes   int64
	CreatedAt   time.Time
	// Encrypted and HasDEK describe whether this copy's row claims the bytes
	// are an encryption envelope and whether the key needed to read them is
	// still present. Carried here so copy-set decisions (which copy to drop)
	// can avoid destroying the last row that can decrypt the object.
	Encrypted bool
	HasDEK    bool
}

// DeletedCopy describes a copy displaced by an overwrite or delete. The
// caller enqueues these for physical orphan cleanup.
type DeletedCopy struct {
	BackendName string
	SizeBytes   int64
}

// -------------------------------------------------------------------------
// PENDING OBJECTS
// -------------------------------------------------------------------------

// PendingObject is an in-flight PUT intent recorded before the backend
// upload. The reaper resolves intents that survive a failed metadata
// commit so a DB outage between PUT and RecordObject cannot silently
// destroy the prior copy of an overwritten key.
type PendingObject struct {
	IntentID      string
	ObjectKey     string
	BackendName   string
	SizeBytes     int64
	Encrypted     bool
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
	ContentHash   string
	CreatedAt     time.Time
}

// PendingPromoteResult describes how PromotePending resolved an intent.
type PendingPromoteResult int

// PendingPromoteCommitted and related constants used by this package.
const (
	// PendingPromoteCommitted means the pending row was promoted into
	// object_locations and removed in the same transaction.
	PendingPromoteCommitted PendingPromoteResult = iota

	// PendingPromoteAmbiguous is reserved for pathological cases the
	// resolver cannot decide between promotion and dropping. The current
	// resolver does not produce this result; the timestamp comparison
	// covers every previously-ambiguous case as Superseded instead. Kept
	// so the metric label and constant stay stable across releases.
	PendingPromoteAmbiguous

	// PendingPromoteAlreadyResolved means the pending row was gone by
	// the time the transaction acquired its lock - another reaper
	// instance already resolved it. Benign no-op.
	PendingPromoteAlreadyResolved

	// PendingPromoteSuperseded means a successful write for the same
	// key committed after this intent was inserted. The intent is
	// provably stale, so the resolver deletes the pending row.
	PendingPromoteSuperseded
)

// -------------------------------------------------------------------------
// QUOTAS AND USAGE
// -------------------------------------------------------------------------

// QuotaStat holds quota statistics for a single backend.
type QuotaStat struct {
	BackendName string
	BytesUsed   int64
	BytesLimit  int64
	OrphanBytes int64
	UpdatedAt   time.Time
}

// UsageLimits holds configurable monthly usage limits for a single backend.
// Zero in a field means unlimited for that dimension.
type UsageLimits struct {
	APIRequestLimit  int64
	EgressByteLimit  int64
	IngressByteLimit int64
}

// UsageStat holds usage statistics for a single backend in a given period.
type UsageStat struct {
	APIRequests  int64
	EgressBytes  int64
	IngressBytes int64
}

// -------------------------------------------------------------------------
// MULTIPART UPLOADS
// -------------------------------------------------------------------------

// MultipartUpload describes an active multipart upload's metadata.
//
// EncryptionKey, KeyID, and Encrypted carry the upload-level wrapped
// DEK shared across every part of an encrypted multipart upload.
// Encrypted is true when EncryptionKey is non-empty. EncryptionKey
// uses the same packed format as MultipartPart.EncryptionKey and
// ObjectLocation.EncryptionKey: encryption.PackKeyData(baseNonce,
// wrappedDEK).
type MultipartUpload struct {
	UploadID      string
	ObjectKey     string
	BackendName   string
	ContentType   string
	Metadata      map[string]string
	Encrypted     bool
	EncryptionKey []byte
	KeyID         string
	CreatedAt     time.Time
}

// MultipartPart describes a single uploaded part of an active upload.
// PartNumber is int (not int32) to match S3 SDK conventions; the
// sqlc row's int32 value is widened by the engine adapter on read.
// UploadID is omitted because parts are always queried in the context
// of a specific upload.
type MultipartPart struct {
	PartNumber    int
	ETag          string
	SizeBytes     int64
	CreatedAt     time.Time
	Encrypted     bool
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
}

// CompletePart is one entry of a client's CompleteMultipartUpload manifest:
// the part it wants assembled and the ETag it believes that part carries.
// Carrying the ETag alongside the number is what lets completion reject a
// stale manifest instead of assembling whatever happens to be stored under
// that number now.
type CompletePart struct {
	PartNumber int
	ETag       string
}

// -------------------------------------------------------------------------
// CLEANUP QUEUE
// -------------------------------------------------------------------------

// CleanupItem represents a pending cleanup operation in the retry queue.
//
// ClaimedAt and ClaimedBy are populated by ClaimPendingCleanups (the worker
// path) and surfaced through GetPendingCleanups (the admin display path);
// both are nil when no worker has ever held the row. Reclaimed is set by
// ClaimPendingCleanups only and is true when this claim recovered a row
// whose previous claim aged past the grace cutoff - the cleanup worker uses
// it to drive the s3o_cleanup_queue_stale_claims_recovered_total metric and
// the cleanup_queue.claim_recovered audit event.
type CleanupItem struct {
	ID          int64
	BackendName string
	ObjectKey   string
	Reason      string
	Attempts    int32
	SizeBytes   int64
	ClaimedAt   *time.Time
	ClaimedBy   *string
	Reclaimed   bool `json:"-"`
}

// CleanupQueueRow is the full payload of a single cleanup_queue row,
// returned by GetCleanupQueueRow inside the move-to-DLQ transaction so
// every column the DLQ insert needs travels with one read.
type CleanupQueueRow struct {
	ID          int64
	BackendName string
	ObjectKey   string
	Reason      string
	SizeBytes   int64
	Attempts    int32
	CreatedAt   time.Time
	LastError   string
}

// CleanupDLQItem is a dead-lettered cleanup row surfaced for operator
// inspection: an object whose backend delete never succeeded within the
// retry budget. FirstEnqueued records when the cleanup was first queued,
// MovedAt when it graduated to the DLQ.
type CleanupDLQItem struct {
	BackendName   string
	ObjectKey     string
	Reason        string
	SizeBytes     int64
	Attempts      int32
	FirstEnqueued time.Time
	MovedAt       time.Time
	LastError     string
}

// -------------------------------------------------------------------------
// NOTIFICATIONS
// -------------------------------------------------------------------------

// NotificationRow represents a pending notification in the outbox table.
type NotificationRow struct {
	ID          int64
	EventType   string
	Payload     []byte
	EndpointURL string
	Attempts    int32
}

// -------------------------------------------------------------------------
// ENCRYPTION ADMIN
// -------------------------------------------------------------------------

// EncryptedLocation represents an encrypted object location for key rotation.
type EncryptedLocation struct {
	ObjectKey     string
	BackendName   string
	EncryptionKey []byte
	KeyID         string
}

// UnencryptedLocation represents an unencrypted object location.
type UnencryptedLocation struct {
	ObjectKey   string
	BackendName string
	SizeBytes   int64
}

// DecryptableLocation represents an encrypted object location with all
// metadata needed for decryption.
type DecryptableLocation struct {
	ObjectKey     string
	BackendName   string
	SizeBytes     int64
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
}

// -------------------------------------------------------------------------
// LISTING RESULTS
// -------------------------------------------------------------------------

// ListObjectsResult holds the result of a list-objects query.
type ListObjectsResult struct {
	Objects               []ObjectLocation
	IsTruncated           bool
	NextContinuationToken string
}

// ListDelimitedResult holds one page of a delimiter-grouped list. Keys whose
// remainder after the prefix contains the delimiter are collapsed into
// CommonPrefixes; the rest are returned as leaf Objects. Truncation and the
// continuation token reflect the merged, key-ordered stream of both.
type ListDelimitedResult struct {
	Objects               []ObjectLocation
	CommonPrefixes        []string
	IsTruncated           bool
	NextContinuationToken string
}

// BuildListPage caps a flat prefix listing at maxKeys and sets the continuation
// token to the last kept key when more objects follow. Shared by both engines so
// the flat ListObjects truncation contract stays identical.
func BuildListPage(objects []ObjectLocation, maxKeys int) *ListObjectsResult {
	result := &ListObjectsResult{}
	if len(objects) > maxKeys {
		result.IsTruncated = true
		result.NextContinuationToken = objects[maxKeys-1].ObjectKey
		result.Objects = objects[:maxKeys]
	} else {
		result.Objects = objects
	}
	return result
}

// DelimitedEntry is one key-ordered row of a delimiter-grouped scan: either a
// CommonPrefix or a leaf object. SkipBound is the value a continuation token
// takes when the page truncates on this entry (the CommonPrefix advanced past
// its group, or the leaf key). Engine adapters scan their loose-index-scan rows
// into these and call BuildDelimitedPage.
type DelimitedEntry struct {
	IsPrefix     bool
	CommonPrefix string
	SkipBound    string
	Leaf         ObjectLocation
}

// BuildDelimitedPage splits the ordered entries into CommonPrefixes and leaf
// objects, caps the page at maxKeys, and sets the continuation token to the last
// kept entry's skip bound when more entries follow. Shared by both engines so
// truncation and token semantics stay identical.
func BuildDelimitedPage(entries []DelimitedEntry, maxKeys int) *ListDelimitedResult {
	result := &ListDelimitedResult{}
	if len(entries) > maxKeys {
		result.IsTruncated = true
		result.NextContinuationToken = entries[maxKeys-1].SkipBound
		entries = entries[:maxKeys]
	}
	for i := range entries {
		if entries[i].IsPrefix {
			result.CommonPrefixes = append(result.CommonPrefixes, entries[i].CommonPrefix)
		} else {
			result.Objects = append(result.Objects, entries[i].Leaf)
		}
	}
	return result
}

// DirEntry holds aggregate stats for one immediate child of a directory
// prefix.
type DirEntry struct {
	Name      string   `json:"name"`
	IsDir     bool     `json:"isDir"`
	FileCount int64    `json:"fileCount"`
	TotalSize int64    `json:"totalSize"`
	Backends  []string `json:"backends"`
	CreatedAt string   `json:"createdAt"`
}

// DirectoryListResult holds the response for a lazy-loaded directory listing.
type DirectoryListResult struct {
	Entries    []DirEntry `json:"entries"`
	HasMore    bool       `json:"hasMore"`
	NextCursor string     `json:"nextCursor"`
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// GroupByKey groups a flat list of object locations into a map keyed by
// object_key.
func GroupByKey(locations []ObjectLocation) map[string][]ObjectLocation {
	m := make(map[string][]ObjectLocation, len(locations)/2)
	for i := range locations {
		m[locations[i].ObjectKey] = append(m[locations[i].ObjectKey], locations[i])
	}
	return m
}
