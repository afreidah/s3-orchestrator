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

// ObjectIdentity is the client-facing view of an object, the same whichever
// copy answers; StoredForm describes the backend bytes instead. ETag is the MD5
// of the bytes the client wrote, or the multipart composite, so it differs from
// the backend's once the bytes are compressed or encrypted.
//
// A nil *ObjectIdentity means unknown, and a read asks the backend. An empty
// ContentType or UserMetadata on a present identity is a real answer.
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
// object a client sees: compression, encryption envelope and key, sizes and
// hash. The zero value describes bytes stored verbatim.
//
// An empty CompressionAlgorithm means not compressed. With both compression and
// encryption on, PlaintextSize is the compressed size the encryptor saw and
// LogicalSize is the size the client wrote. CompressionLevel does not affect
// decoding.
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
}

// Unreadable reports whether the form describes an envelope with no key to
// open it, which is how import records encrypted bytes no surviving row can
// decrypt.
func (f *StoredForm) Unreadable() bool {
	return f != nil && f.Encrypted && len(f.EncryptionKey) == 0
}

// ObjectLocation records that a backend currently holds a copy of a key,
// along with the size and any encryption or integrity metadata.
//
// StorageKey is the path this copy occupies on its backend; a write stores its
// bytes under the object key plus its intent id (see internalkey.StorageKey),
// so two writes of one key never share a path.
//
// Unmanaged marks bytes that count toward quota but that no worker touches and
// clients cannot list or read: an object outside every bucket prefix, or an
// imported envelope no key can open. Its zero value means managed.
//
// LastScrubbedAt is nil for a copy never verified, and also on rows from
// queries that do not select it; the compression columns are likewise zero
// when not selected. CompressionProbeSize and CompressionProbeLevel record what
// the encoder measured for a copy it declined, so a later pass need not
// download it again.
//
// TagCount is how many tags the key carries. Tags belong to the key, so every
// copy reports the same count. Only GetAllObjectLocations fills it in, for the
// tagging-count header on the read path.
type ObjectLocation struct {
	ObjectKey                string
	BackendName              string
	StorageKey               string
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
	TagCount                 int
}

// ExistingCopy is the projection of an object_locations row that promotion
// and overwrite logic needs from a SELECT-for-update read.
//
// Encrypted and HasDEK let a caller choosing which copy to drop avoid
// destroying the last row able to decrypt the object.
type ExistingCopy struct {
	BackendName string
	StorageKey  string
	SizeBytes   int64
	CreatedAt   time.Time
	Encrypted   bool
	HasDEK      bool
}

// DeletedCopy describes bytes that need removing from their backend: a copy
// displaced by an overwrite or delete, or a superseded intent's upload.
// StorageKey is the exact path to delete; never derive it from the object key,
// or a late cleanup could remove a newer write's bytes. Reason is the
// cleanup-queue label used on retry; empty means the caller's default.
type DeletedCopy struct {
	BackendName string
	StorageKey  string
	SizeBytes   int64
	Reason      string
}

// Cleanup is the request that deletes this copy of objectKey, labelled with the
// copy's own reason or defaultReason when it has none.
func (d DeletedCopy) Cleanup(objectKey, defaultReason string) *CleanupRequest {
	reason := d.Reason
	if reason == "" {
		reason = defaultReason
	}
	return &CleanupRequest{
		BackendName: d.BackendName,
		ObjectKey:   objectKey,
		StorageKey:  d.StorageKey,
		Reason:      reason,
		SizeBytes:   d.SizeBytes,
	}
}

// CleanupReasonSupersededIntent labels bytes of an intent a newer write
// cleared. CleanupReasonCompanionDiscarded labels an extra-copy intent the
// reaper could not vouch for. CleanupReasonCompanionUntrusted labels an extra
// copy that finished after a newer write took the key. None was committed, so
// the backend may not hold the bytes at all.
const (
	CleanupReasonSupersededIntent   = "superseded_intent"
	CleanupReasonCompanionDiscarded = "companion_discarded"
	CleanupReasonCompanionUntrusted = "companion_untrusted"
)

// -------------------------------------------------------------------------
// PENDING OBJECTS
// -------------------------------------------------------------------------

// SupersededIntent is an intent a write cleared because it replaced the object
// the intent was for. The backend and size are what the removal of its bytes
// needs; the intent itself is already gone by the time a caller sees this.
type SupersededIntent struct {
	IntentID    string
	BackendName string
	StorageKey  string
	SizeBytes   int64
}

// PendingRole says what promoting an intent means. A primary intent is the
// write itself, so promoting it replaces whatever the key held. A companion
// intent is one of the further copies a write places at the same time, so it
// must never displace the copies its siblings committed.
type PendingRole string

// The two meanings an intent can carry. An empty role reads as primary, which
// is what every intent written before roles existed meant.
const (
	PendingRolePrimary   PendingRole = "primary"
	PendingRoleCompanion PendingRole = "companion"
)

// PendingObject is an in-flight PUT intent recorded before the backend
// upload. The reaper resolves intents that survive a failed metadata
// commit so a DB outage between PUT and RecordObject cannot silently
// destroy the prior copy of an overwritten key. StorageKey, minted from the key
// and the intent id, is recorded up front so every resolver addresses exactly
// the bytes this write placed.
type PendingObject struct {
	IntentID                 string
	ObjectKey                string
	StorageKey               string
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
	Role                     PendingRole
}

// IsCompanion reports whether promoting this intent adds a copy rather than
// replacing the key's copy set.
func (p *PendingObject) IsCompanion() bool {
	return p.Role == PendingRoleCompanion
}

// RoleOrDefault names the role to store, so a caller that left it unset writes
// the same value the column defaults to rather than an empty string the CHECK
// constraint would reject.
func (p *PendingObject) RoleOrDefault() PendingRole {
	if p.Role == "" {
		return PendingRolePrimary
	}
	return p.Role
}

// PendingPromoteResult describes how PromotePending resolved an intent.
type PendingPromoteResult int

// The outcomes of promoting a pending write intent. Ambiguous is never
// produced but keeps its value so metric labels stay stable. The companion
// outcomes never record a copy; after Discarded, replication rebuilds it.
const (
	PendingPromoteCommitted          PendingPromoteResult = iota // promoted and the intent removed, one transaction
	PendingPromoteAmbiguous                                      // reserved; see above
	PendingPromoteAlreadyResolved                                // another reaper got there first, a benign no-op
	PendingPromoteSuperseded                                     // a later write for the key committed, so the intent is provably stale
	PendingPromoteCompanionKept                                  // an extra-copy intent whose backend already holds a recorded copy
	PendingPromoteCompanionDiscarded                             // an extra-copy intent whose bytes are unaccounted for and now removed
)

// CompanionCommitResult describes how an upload that outlived its response
// settled the copy it was placing. Untrusted is not a failure: a newer write
// took the key mid-upload, so the bytes are stale.
type CompanionCommitResult int

// The outcomes of committing an extra copy after the client has been answered.
const (
	CompanionCopyCommitted CompanionCommitResult = iota // the intent was still there, so the copy is recorded
	CompanionCopyUntrusted                              // a newer write cleared the intent; the copy is dropped and replication rebuilds it
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
// keyed by backend name. The caller applies it to the in-memory counter so the
// transaction never locks the backend_quotas row.
type QuotaDeltas map[string]int64

// Add accumulates a signed delta for one backend. A nil map is left alone, so
// a path that never allocated one is not forced to.
func (q QuotaDeltas) Add(backendName string, delta int64) {
	if q == nil {
		return
	}
	q[backendName] += delta
}

// BackendQuotaUsage is one backend's quota row plus the bytes that occupy it
// without appearing in bytes_used: orphans awaiting cleanup and incomplete
// multipart parts. It is the baseline the in-memory quota tracker adds its
// unflushed deltas to.
type BackendQuotaUsage struct {
	BackendName   string
	BytesLimit    int64
	BytesUsed     int64
	OrphanBytes   int64
	InflightBytes int64
}

// DrainState is where a backend's drain stands. A backend in any of these
// states is refused new writes.
type DrainState string

// Drain states. Draining is in progress, drained has moved every object off,
// and failed stopped on an error and waits for an operator to retry or clear it.
const (
	DrainStateDraining DrainState = "draining"
	DrainStateDrained  DrainState = "drained"
	DrainStateFailed   DrainState = "failed"
)

// BackendDrain is one backend's drain record. FinishedAt is nil while the drain
// is in progress, and LastError is set only for a failed drain.
type BackendDrain struct {
	BackendName  string
	State        DrainState
	ObjectsMoved int64
	LastError    string
	StartedAt    time.Time
	FinishedAt   *time.Time
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
// EncryptionKey is the upload-level wrapped DEK shared by every part, in the
// encryption.PackKeyData format; Encrypted is true when it is non-empty.
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

// MultipartPart describes a single uploaded part of an active upload. ETag is
// what the backend returned for the stored part; PlaintextETag is the MD5 of
// the client's bytes and is the one that builds the composite ETag. Older
// parts may have an empty PlaintextETag.
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

// CleanupRequest is one deletion handed to the retry queue. The worker deletes
// StorageKey; ObjectKey tells an operator what the orphan was. SizeBytes is
// credited back to orphan_bytes when the delete lands.
type CleanupRequest struct {
	BackendName string
	ObjectKey   string
	StorageKey  string
	Reason      string
	SizeBytes   int64
}

// CleanupItem represents a pending cleanup operation in the retry queue. The
// worker deletes StorageKey; ObjectKey names the object for display.
// ClaimedAt and ClaimedBy are nil when no worker has held the row. Reclaimed is
// set only by ClaimPendingCleanups, when the claim recovered a row whose
// previous claim aged past the grace cutoff.
type CleanupItem struct {
	ID          int64
	BackendName string
	ObjectKey   string
	StorageKey  string
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
	StorageKey  string
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
	StorageKey    string
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

// CoverageStat says how far behind integrity verification is.
// OldestUnverifiedAge and NeverVerified cover only copies the sweep can read,
// since an unreachable copy would pin the age forever. Deferred counts the
// excluded copies so a fleet mostly over its usage limit does not read as
// healthy.
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

// UnencryptedLocation represents an unencrypted object location. Etag is the
// value seen at listing time; the conversion commits only while the row still
// reports it, so a client write mid-pass aborts the commit.
type UnencryptedLocation struct {
	ObjectKey   string
	BackendName string
	StorageKey  string
	SizeBytes   int64
	Etag        string
}

// DecryptableLocation represents an encrypted object location with all
// metadata needed for decryption. Etag works as on UnencryptedLocation.
type DecryptableLocation struct {
	ObjectKey     string
	BackendName   string
	StorageKey    string
	SizeBytes     int64
	EncryptionKey []byte
	KeyID         string
	PlaintextSize int64
	Etag          string
}

// Cursor is the position a paged admin listing resumes from: the last
// (object_key, backend_name) it returned. The zero value starts at the
// beginning. Bulk rewrite passes page by cursor because the rows they rewrite
// leave the listing's predicate, which would make an offset skip rows.
type Cursor struct {
	ObjectKey   string
	BackendName string
}

// SizeCursor is the position a backend's smallest-first listing resumes from:
// the last (size_bytes, object_key) it returned. An object key appears once per
// backend, so it breaks ties between copies of equal size. The zero value
// starts at the beginning.
type SizeCursor struct {
	SizeBytes int64
	ObjectKey string
}

// CompressionStat reports the encoded copies on one backend, their logical
// size and what they occupy; the saving is LogicalBytes - StoredBytes. Copies
// stored verbatim are excluded.
type CompressionStat struct {
	Objects      int64
	LogicalBytes int64
	StoredBytes  int64
}

// RewritableLocation is one copy a bulk compression pass may rewrite. It
// carries encryption metadata because an encrypted copy must be decrypted
// before encoding and re-encrypted afterwards. An empty CompressionAlgorithm
// means not encoded. Etag works as on UnencryptedLocation.
type RewritableLocation struct {
	ObjectKey                string
	BackendName              string
	StorageKey               string
	SizeBytes                int64
	Encrypted                bool
	EncryptionKey            []byte
	KeyID                    string
	PlaintextSize            int64
	CompressionAlgorithm     string
	CompressionLevel         string
	CompressionFormatVersion int
	LogicalSize              int64
	Etag                     string
}

// CompressionThresholds are the settings that decide whether a copy is worth
// encoding, passed to the uncompressed listing so it selects only candidates.
// Recorded probes are judged against the current values, so loosening a
// threshold returns declined copies to the pass. A probe counts against
// MinRatio only if it was taken at Level.
type CompressionThresholds struct {
	MinSize  int64
	MinRatio float64
	Level    string
}

// CompressionProbe is what the encoder measured for a copy it declined to store
// compressed: the size it produced and the level it used. A zero Size means
// never probed. Only ratio declines are recorded, since other declines are
// answered without encoding.
type CompressionProbe struct {
	ObjectKey   string
	BackendName string
	Size        int64
	Level       string
}

// CompressedUpdate is the new description of a copy a compression pass has
// rewritten. SizeBytes is what now occupies the backend, PlaintextSize is what
// the encryptor was handed, and LogicalSize is the object the client wrote. An
// empty Algorithm records the copy as no longer encoded. EncryptionKey and
// KeyID are required when the copy was re-encrypted, since re-encryption mints
// a new nonce and wrapped key, and empty for an unencrypted copy.
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
	ExpectedEtag  string
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
	ExpectedEtag   string
}

// DecryptedUpdate is the new description of a copy a decryption pass has
// rewritten back to plaintext. PlaintextSize is what now occupies the backend,
// and the envelope columns the row still holds are cleared.
type DecryptedUpdate struct {
	ObjectKey     string
	BackendName   string
	PlaintextSize int64
	ExpectedEtag  string
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
