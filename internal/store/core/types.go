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
	return f.Form().Unreadable()
}

// ObjectLocation records that a backend currently holds a copy of a key,
// along with the size and the stored form its bytes carry.
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
	StoredForm
	ObjectKey             string
	BackendName           string
	StorageKey            string
	SizeBytes             int64
	CreatedAt             time.Time
	CompressionProbeSize  int64
	CompressionProbeLevel string
	LastScrubbedAt        *time.Time
	Unmanaged             bool
	Identity              *ObjectIdentity
	TagCount              int
}

// ObjectSize is the size of the object the client wrote: logical_size for an
// encoded copy, plaintext_size for an encrypted one, and the stored size
// otherwise. It is the Go half of the definition object_locations.object_size
// generates, and the two must agree.
func (l *ObjectLocation) ObjectSize() int64 {
	return l.ClientSize(l.SizeBytes)
}

// ClientSize is the size of the bytes the client wrote, for stored bytes of
// size stored carrying this form: the logical size under an encoding, the
// plaintext size under an envelope, and stored itself otherwise.
func (f *StoredForm) ClientSize(stored int64) int64 {
	switch form := f.Form(); {
	case form.Has(StoredCompressed):
		return f.LogicalSize
	case form.Has(StoredEncrypted):
		return f.PlaintextSize
	default:
		return stored
	}
}

// CopyAt returns this copy as it reads once its bytes are copied verbatim to
// storageKey on backend. Every other column carries over, including the write
// time clients see as Last-Modified, because the bytes are the same bytes.
func (l *ObjectLocation) CopyAt(backend, storageKey string) *ObjectLocation {
	dest := *l
	dest.BackendName = backend
	dest.StorageKey = storageKey
	return &dest
}

// ExistingCopy is the projection of an object_locations row that promotion
// and overwrite logic needs from a SELECT-for-update read.
//
// Form lets a caller choosing which copy to drop avoid destroying the last row
// able to decrypt the object.
type ExistingCopy struct {
	BackendName string
	StorageKey  string
	SizeBytes   int64
	CreatedAt   time.Time
	Form        StoredFlags
}

// removedCopy describes bytes a transaction stopped recording: a copy displaced
// by an overwrite or delete, or the upload of an intent it cleared. StorageKey
// is the exact path to delete; never derive it from the object key, or a late
// cleanup could remove a newer write's bytes. Reason overrides the label the
// transaction queues the bytes under; empty means its default.
type removedCopy struct {
	BackendName string
	StorageKey  string
	SizeBytes   int64
	Reason      string
}

// request is the cleanup that deletes this copy of objectKey, labelled with the
// copy's own reason or defaultReason when it has none.
func (d removedCopy) request(objectKey, defaultReason string) *CleanupRequest {
	reason := d.Reason
	if reason == "" {
		reason = defaultReason
	}
	return &CleanupRequest{
		BackendName: d.BackendName,
		ObjectKey:   objectKey,
		StorageKey:  StoragePath(objectKey, d.StorageKey),
		Reason:      reason,
		SizeBytes:   d.SizeBytes,
	}
}

// The labels the store queues bytes under when the caller does not name one.
// CleanupReasonSupersededIntent labels bytes of an intent a newer write
// cleared. CleanupReasonCompanionDiscarded labels an extra-copy intent the
// reaper could not vouch for. CleanupReasonCompanionUntrusted labels an extra
// copy that finished after a newer write took the key. None of those three
// was committed, so the backend may not hold the bytes at all.
// CleanupReasonPartReplaced labels a part a re-upload of its number replaced.
const (
	CleanupReasonOverwritten        = "overwrite_displaced"
	CleanupReasonDeleted            = "object_deleted"
	CleanupReasonPartReplaced       = "part_replaced"
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
	StoredForm
	IntentID    string
	ObjectKey   string
	StorageKey  string
	BackendName string
	SizeBytes   int64
	Identity    *ObjectIdentity
	CreatedAt   time.Time
	Role        PendingRole
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

// QuotaStat is one backend's byte ceiling and what occupies it: the striped
// byte total, orphans awaiting cleanup, and the writes and multipart parts that
// have not landed in bytes_used yet. It is read from backend_capacity, the view
// admission tests against.
type QuotaStat struct {
	BackendName   string
	BytesUsed     int64
	BytesLimit    int64
	OrphanBytes   int64
	InflightBytes int64
}

// Unlimited reports whether the backend has no byte ceiling. A zero
// bytes_limit is how the schema spells "no quota enforcement".
func (q QuotaStat) Unlimited() bool {
	return q.BytesLimit <= 0
}

// Occupied is the byte total a write is judged against: what the ledger has
// recorded, plus what is on the backend but not yet recorded.
func (q QuotaStat) Occupied() int64 {
	return q.BytesUsed + q.OrphanBytes + q.InflightBytes
}

// quotaDeltas is the signed per-backend byte change a mutation makes, keyed by
// backend name, gathered so chargeStripes can write each backend once.
type quotaDeltas map[string]int64

// add accumulates a signed delta for one backend.
func (q quotaDeltas) add(backendName string, delta int64) {
	q[backendName] += delta
}

// BackendStateRecord is one backend's state record. ObjectsMoved, LastError,
// StartedAt and FinishedAt describe the backend's most recent drain:
// FinishedAt is nil while it is in progress, and LastError is set only when it
// failed.
type BackendStateRecord struct {
	BackendName  string
	Flags        BackendFlags
	ObjectsMoved int64
	LastError    string
	StartedAt    time.Time
	FinishedAt   *time.Time
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
// parts may have an empty PlaintextETag. Parts are never compressed, so the
// stored form carries only the envelope. StorageKey is the path the upload of
// this part wrote; it is empty for a part recorded before each upload had its
// own path, which sits at the part number's fixed path.
type MultipartPart struct {
	StoredForm
	PartNumber    int
	ETag          string
	PlaintextETag string
	SizeBytes     int64
	StorageKey    string
	CreatedAt     time.Time
}

// Path returns where the part's bytes are stored on the upload's backend.
func (p *MultipartPart) Path(uploadID string) string {
	return StoragePath(PartPath(uploadID, p.PartNumber), p.StorageKey)
}

// RecordPartParams is one RecordPart call's inputs. Bundled rather than passed
// positionally: the two ETags and the two sizes are adjacent values of the same
// types, which is exactly the shape a transposition hides in. Intent is the
// claim the part's upload holds on the upload's backend; its path is where the
// bytes were written, and recording the part clears it. A completion that
// renewed its mark after CompletionStaleBefore refuses the part.
type RecordPartParams struct {
	UploadID              string
	PartNumber            int
	ETag                  string
	PlaintextETag         string
	SizeBytes             int64
	Form                  *StoredForm
	Intent                *PendingObject
	CompletionStaleBefore time.Time
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

// CleanupRequest is one deletion a transaction queues. StorageKey is what is
// deleted; ObjectKey tells an operator what the bytes belonged to. SizeBytes
// counts as the backend's orphan bytes until the row is retired.
type CleanupRequest struct {
	BackendName string
	ObjectKey   string
	StorageKey  string
	Reason      string
	SizeBytes   int64
}

// CleanupItem represents a pending cleanup operation in the retry queue. The
// worker deletes StorageKey; ObjectKey names the object for display. Claim is
// nil when no instance holds the row. Reclaimed is set only by
// ClaimPendingCleanups, when the claim recovered a row whose previous claim
// aged past the grace cutoff.
type CleanupItem struct {
	ID          int64
	BackendName string
	ObjectKey   string
	StorageKey  string
	Reason      string
	Attempts    int32
	SizeBytes   int64
	Claim       *CleanupClaim
	Reclaimed   bool `json:"-"`
}

// CleanupClaim is the hold one instance has on a cleanup row: when it was
// stamped and by which instance, empty for a process that has no instance id.
// Every write that settles a row matches it, so an instance whose claim aged
// out and was reclaimed by another cannot settle the row a second time.
type CleanupClaim struct {
	At time.Time
	By string
}

// MaxCleanupAttempts is the retry ceiling. The 1-minute starting backoff
// doubled 10 times yields ~17 hours of total retry runway, which is enough to
// bridge the longest realistic backend outages. A row whose next attempt would
// reach it graduates to cleanup_dlq for operator action.
const MaxCleanupAttempts = 10

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

// FormFilter selects copies by the layers their bytes carry: every flag of With
// and none of Without. Backend and KeyID narrow the page to one value when set,
// ExceptKeyID leaves out copies under that key, and ManagedOnly leaves out data
// the orchestrator does not manage. Candidates, when set, keeps only copies a
// compression pass would still try to encode.
type FormFilter struct {
	With        StoredFlags
	Without     StoredFlags
	Backend     string
	KeyID       string
	ExceptKeyID string
	ManagedOnly bool
	Candidates  *CompressionThresholds
}

// CompressionThresholds are the settings that decide whether a copy is worth
// encoding, passed to the copy listing so it selects only candidates. Recorded
// probes are judged against the current values, so loosening a threshold
// returns declined copies to the pass. A probe counts against MinRatio only if
// it was taken at Level.
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
	Size  int64
	Level string
}

// CopyPatch is a metadata write to one copy. Each field set is written and
// the rest are left as they are: EncryptionKey and KeyID together re-wrap the
// DEK, ContentHash records a backfilled hash, Scrubbed stamps the copy as
// examined now, and Probe records a compression decline.
type CopyPatch struct {
	EncryptionKey []byte
	KeyID         string
	ContentHash   string
	Scrubbed      bool
	Probe         *CompressionProbe
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

// DirStat is one aggregated child of a directory prefix: a sub-directory
// roll-up, or a file directly beneath the prefix. TotalSize sums every
// replica, so a directory reports the physical storage it takes.
type DirStat struct {
	Name      string
	IsDir     bool
	FileCount int64
	TotalSize int64
}

// DirFile is one file directly beneath a directory prefix: the backends
// holding it, one replica's size, and when it was written.
type DirFile struct {
	ObjectKey string
	Backends  []string
	SizeBytes int64
	CreatedAt time.Time
}

// dirTimeFormat is how a directory listing renders a file's write time.
const dirTimeFormat = "2006-01-02 15:04"

// BuildDirectoryListing merges the roll-ups for a prefix with one page of file
// detail, fetched one row past maxKeys to detect truncation. A file outside
// the page is left out, and a file reports one replica's size, which is what
// the client uploaded, rather than the replica sum.
func BuildDirectoryListing(prefix string, stats []DirStat, files []DirFile, maxKeys int) *DirectoryListResult {
	result := &DirectoryListResult{Entries: make([]DirEntry, 0, len(stats))}
	if len(files) > maxKeys {
		files = files[:maxKeys]
		result.HasMore = true
		result.NextCursor = files[len(files)-1].ObjectKey
	}
	byName := make(map[string]*DirFile, len(files))
	for i := range files {
		byName[files[i].ObjectKey[len(prefix):]] = &files[i]
	}
	for _, s := range stats {
		entry := DirEntry{Name: prefix + s.Name, IsDir: s.IsDir, FileCount: s.FileCount, TotalSize: s.TotalSize}
		if !s.IsDir {
			f, ok := byName[s.Name]
			if !ok {
				continue
			}
			entry.Backends, entry.TotalSize = f.Backends, f.SizeBytes
			entry.CreatedAt = f.CreatedAt.Format(dirTimeFormat)
		}
		result.Entries = append(result.Entries, entry)
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
