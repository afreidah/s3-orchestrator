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

// ObjectIdentity is what the client sees an object as, independent of which
// copy answers: the validator conditional requests compare against, the
// content type it was written with, and the user metadata it carries. The
// counterpart to StoredForm, which describes the bytes on the backend instead.
//
// ETag is the MD5 of the bytes the client wrote, or the AWS composite for a
// multipart upload, and is therefore not what the backend reports once the
// stored bytes are compressed or encrypted.
//
// A nil *ObjectIdentity means unknown - a row written before identity was
// recorded, or an object imported from a backend - and a read falls back to
// asking the backend. That is distinct from a present identity with an empty
// ContentType or an empty UserMetadata, which are answers in themselves.
type ObjectIdentity struct {
	ETag         string
	ContentType  string
	UserMetadata map[string]string
}

// Complete reports whether the identity answers a HEAD on its own. An identity
// missing the ETag cannot: the response has to carry a validator, and the only
// place left to get one is the backend.
func (i *ObjectIdentity) Complete() bool {
	return i != nil && i.ETag != ""
}

// StoredForm describes how the bytes on a backend differ from the logical
// object a client sees: whether they are compressed, whether they are an
// encryption envelope, the key needed to read them back, and the sizes and
// hash they reduce to. The write path produces one per stored copy and the
// store records it; the read path needs it to serve the object correctly.
//
// An empty CompressionAlgorithm means the bytes are not compressed, so there
// is no separate flag that could drift out of step with it. LogicalSize is the
// size of the object the client wrote and differs from PlaintextSize once both
// features are on: the stored bytes are then ciphertext of compressed data, so
// PlaintextSize is the pre-encryption (compressed) size while LogicalSize is
// the original. CompressionLevel does not affect decoding and is carried for
// diagnostics and for rewrite passes.
//
// The zero value describes bytes stored verbatim, which is also why Unmanaged
// is negated: it is stored as the `managed` column, so a construction site that
// omits the field cannot accidentally produce a row every worker ignores.
type StoredForm struct {
	Encrypted                bool
	EncryptionKey            []byte
	KeyID                    string
	PlaintextSize            int64
	ContentHash              string
	CompressionAlgorithm     string
	CompressionLevel         string
	CompressionFormatVersion int
	LogicalSize              int64

	Unmanaged bool // on the backend but outside every virtual bucket prefix: real bytes no worker acts on
}

// ObjectLocation records that a backend currently holds a copy of a key,
// along with the size and any encryption or integrity metadata.
//
// LastScrubbedAt is nil for a copy that has never been verified, which is a
// different state from one verified long ago and has to stay distinguishable.
// It is also nil on rows from queries that do not select the column, so a
// caller reading it wants a query that does.
//
// Unmanaged marks an object that exists on the backend but outside every
// configured virtual bucket prefix: real bytes the orchestrator did not write
// and does not act on. It is stored as the negated `managed` column, so the
// zero value means managed and a construction site that omits it cannot
// accidentally produce a row the workers ignore.
//
// The compression columns follow StoredForm: an empty algorithm means the
// bytes are stored verbatim, and they are zero on rows from queries that do
// not select them.
//
// CompressionProbeSize and CompressionProbeLevel are bookkeeping about the copy
// rather than a description of its content, like LastScrubbedAt: they record
// what the encoder measured for a copy it declined, so a later pass need not
// download it to measure again.
type ObjectLocation struct {
	ObjectKey                string
	BackendName              string
	SizeBytes                int64
	CreatedAt                time.Time
	Encrypted                bool
	EncryptionKey            []byte
	KeyID                    string
	PlaintextSize            int64
	ContentHash              string
	CompressionAlgorithm     string
	CompressionLevel         string
	CompressionFormatVersion int
	LogicalSize              int64
	CompressionProbeSize     int64
	CompressionProbeLevel    string
	LastScrubbedAt           *time.Time
	Unmanaged                bool
	Identity                 *ObjectIdentity
}

// ExistingCopy is the projection of an object_locations row that promotion
// and overwrite logic needs from a SELECT-for-update read.
//
// Encrypted and HasDEK say whether the row claims the bytes are an envelope and
// whether the key that reads them is still present. They are carried here so a
// decision about which copy to drop cannot destroy the last row able to decrypt
// the object.
type ExistingCopy struct {
	BackendName string
	SizeBytes   int64
	CreatedAt   time.Time
	Encrypted   bool
	HasDEK      bool
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
	IntentID                 string
	ObjectKey                string
	BackendName              string
	SizeBytes                int64
	Encrypted                bool
	EncryptionKey            []byte
	KeyID                    string
	PlaintextSize            int64
	ContentHash              string
	CompressionAlgorithm     string
	CompressionLevel         string
	CompressionFormatVersion int
	LogicalSize              int64
	Identity                 *ObjectIdentity
	CreatedAt                time.Time
}

// PendingPromoteResult describes how PromotePending resolved an intent.
type PendingPromoteResult int

// The outcomes of promoting a pending write intent.
//
// Ambiguous is reserved and never produced: the timestamp comparison resolves
// every case it was meant for as Superseded instead. It stays declared so the
// metric label and the constant keep their values across releases.
const (
	PendingPromoteCommitted       PendingPromoteResult = iota // promoted and the intent removed, one transaction
	PendingPromoteAmbiguous                                   // reserved; see above
	PendingPromoteAlreadyResolved                             // another reaper got there first, a benign no-op
	PendingPromoteSuperseded                                  // a later write for the key committed, so the intent is provably stale
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

// QuotaDeltas is the signed per-backend byte change a committed mutation made,
// keyed by backend name.
//
// Returned to the caller rather than written inside the transaction: the
// counter these feed is held in memory and flushed on an interval, and a
// transaction that also updated backend_quotas would hold that row's lock
// until commit, which is what every concurrent write to a backend used to
// serialize on.
type QuotaDeltas map[string]int64

// Add accumulates a signed delta for one backend. A nil map is left alone, so
// a path that never allocated one is not forced to.
func (q QuotaDeltas) Add(backendName string, delta int64) {
	if q == nil {
		return
	}
	q[backendName] += delta
}

// BackendQuotaUsage is one backend's quota row together with the bytes that
// occupy the backend without appearing in bytes_used: orphans awaiting cleanup,
// and the parts of multipart uploads that have not completed. Both are
// subtracted when a write target is chosen, so a view carrying only the row
// would route to a backend that is fuller than it reports.
//
// This is the baseline the in-memory quota tracker holds between flushes, which
// is why it is a value rather than a live query: the tracker adds its own
// unflushed delta on top of it.
type BackendQuotaUsage struct {
	BackendName   string
	BytesLimit    int64
	BytesUsed     int64
	OrphanBytes   int64
	InflightBytes int64
}

// Unlimited reports whether the backend has no byte ceiling. A zero
// bytes_limit is how the schema spells "no quota enforcement".
func (b BackendQuotaUsage) Unlimited() bool {
	return b.BytesLimit <= 0
}

// Occupied is the byte total a write is judged against: what the ledger has
// recorded, plus what is on the backend but not yet recorded.
func (b BackendQuotaUsage) Occupied() int64 {
	return b.BytesUsed + b.OrphanBytes + b.InflightBytes
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
	Tags          []Tag
	CreatedAt     time.Time
}

// MultipartPart describes a single uploaded part of an active upload.
// PartNumber is int (not int32) to match S3 SDK conventions; the
// sqlc row's int32 value is widened by the engine adapter on read.
// UploadID is omitted because parts are always queried in the context
// of a specific upload.
// ETag is what the backend returned for the part as stored; PlaintextETag is
// the MD5 of the bytes the client sent for it. They differ once the stored
// part is an encryption envelope, and only the second one can build the
// object's composite ETag. PlaintextETag is empty for parts uploaded before it
// was recorded.
type MultipartPart struct {
	PartNumber    int
	ETag          string
	PlaintextETag string
	SizeBytes     int64
	CreatedAt     time.Time
	Encrypted     bool
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
}

// RecordPartParams is one RecordPart call's inputs. Bundled rather than passed
// positionally: the two ETags and the two sizes are adjacent values of the same
// types, which is exactly the shape a transposition hides in.
type RecordPartParams struct {
	UploadID      string
	PartNumber    int
	ETag          string
	PlaintextETag string
	SizeBytes     int64
	Form          *StoredForm
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
// INTEGRITY COVERAGE
// -------------------------------------------------------------------------

// CoverageStat says how far behind integrity verification is, split by whether
// the sweep can reach the copy at all.
//
// OldestUnverifiedAge and NeverVerified describe reachable copies only. A copy
// on a backend the sweep may not read can never be stamped, so counting it in
// the age pins that figure to a fixed timestamp and it then climbs by a day
// every day no matter how much is verified. Deferred is the count it excludes,
// reported rather than dropped so a fleet holding most of its copies on a
// backend over its usage limit cannot read as healthy.
type CoverageStat struct {
	OldestUnverifiedAge time.Duration
	NeverVerified       int64
	Deferred            int64
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

// Cursor is the position a paged admin listing resumes from: the last
// (object_key, backend_name) it returned. The zero value starts at the
// beginning.
//
// It exists because the bulk rewrite passes mutate the rows they walk. Each
// object a pass rewrites leaves the predicate its listing selects on, so the
// set shrinks mid-walk; an offset advanced against that steps over the rows
// that moved up and the run reports success having skipped them. A cursor names
// a row rather than a position, so rows leaving the set behind it change
// nothing.
type Cursor struct {
	ObjectKey   string
	BackendName string
}

// CompressionStat reports what compression is worth on one backend: how many
// copies are stored encoded, what those objects are, and what they occupy.
//
// The saving is LogicalBytes - StoredBytes, derived rather than stored so it
// cannot disagree with the two figures it comes from. Copies stored verbatim
// are excluded: counting them would report a ratio no encoder produced.
type CompressionStat struct {
	Objects      int64
	LogicalBytes int64
	StoredBytes  int64
}

// RewritableLocation is one copy a bulk compression pass may rewrite. It
// carries the encryption metadata as well, because compression sits inside
// encryption: an encrypted copy has to be decrypted before its bytes can be
// encoded, and re-encrypted afterwards under the same key.
//
// An empty CompressionAlgorithm means the stored bytes are not encoded, which
// is what the compress direction selects on and the decompress direction
// excludes.
type RewritableLocation struct {
	ObjectKey                string
	BackendName              string
	SizeBytes                int64
	Encrypted                bool
	EncryptionKey            []byte
	KeyID                    string
	PlaintextSize            int64
	CompressionAlgorithm     string
	CompressionLevel         string
	CompressionFormatVersion int
	LogicalSize              int64
}

// CompressionThresholds are the settings that decide whether a copy is worth
// encoding, passed to the uncompressed listing so it selects only candidates.
//
// The listing applies them rather than the pass filtering afterwards, because
// both answers are durable: a copy under MinSize is never a candidate, and a
// copy already measured as unable to reach MinRatio stays declined until one of
// these values changes. Judging a recorded measurement against the current
// settings is what lets a loosened threshold return those copies to the pass
// with no read at all.
//
// Level names the level a recorded measurement must have been taken at to count
// against MinRatio. A measurement from a different level describes an encoding
// the pass would no longer produce.
type CompressionThresholds struct {
	MinSize  int64
	MinRatio float64
	Level    string
}

// CompressionProbe is what the encoder measured for a copy it declined to store
// compressed: the size it produced and the level it produced it at.
//
// A zero Size means the copy has never been probed. Only the ratio decision is
// recorded, because it is the only one that costs a download and an encode to
// reach: a size floor is answered from the row, and a copy declined by usage
// limits never reached the encoder.
type CompressionProbe struct {
	ObjectKey   string
	BackendName string
	Size        int64
	Level       string
}

// CompressedUpdate is the new description of a copy a compression pass has
// rewritten. SizeBytes is what now occupies the backend, PlaintextSize is what
// the encryptor was handed (the encoded stream, when the copy is encrypted),
// and LogicalSize is the object the client wrote.
//
// A zero Algorithm records the copy as no longer encoded, which is what the
// decompress direction writes.
//
// EncryptionKey and KeyID are set when the rewrite re-encrypted the copy, and
// they are not optional there: re-encryption produces a new base nonce and a
// new wrapped key, so a row still holding the old ones describes bytes nothing
// can decrypt. They are empty for a copy that was never encrypted.
type CompressedUpdate struct {
	ObjectKey     string
	BackendName   string
	Algorithm     string
	Level         string
	FormatVersion int
	SizeBytes     int64
	PlaintextSize int64
	LogicalSize   int64
	EncryptionKey []byte
	KeyID         string
}

// EncryptedUpdate is the new description of a copy an encryption pass has
// rewritten. PlaintextSize is what the encryptor was handed and CiphertextSize
// is what now occupies the backend, so the difference between them is what the
// envelope cost and what the backend's counter has to move by.
type EncryptedUpdate struct {
	ObjectKey      string
	BackendName    string
	EncryptionKey  []byte
	KeyID          string
	PlaintextSize  int64
	CiphertextSize int64
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
