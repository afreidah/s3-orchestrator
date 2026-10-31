// -------------------------------------------------------------------------------
// Metrics  -  Encryption, Integrity Verification
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface. Split out of the
// original 784-line metrics.go to keep each subsystem under ~150 lines.
// -------------------------------------------------------------------------------

package telemetry

// Encryption operation and error metrics.
var (
	// EncryptionOpsTotal counts encryption operations by type.
	EncryptionOpsTotal = counterVec("s3o_encryption_operations_total", "Total encryption operations", "op")

	// EncryptionErrorsTotal counts encryption errors by operation and type.
	EncryptionErrorsTotal = counterVec("s3o_encryption_errors_total", "Total encryption errors", "op", "error_type")

	// EncryptionUnknownKeyIDTotal counts decryption attempts refused because
	// the keyID was not found in the configured keys.
	EncryptionUnknownKeyIDTotal = counter("s3o_encryption_unknown_key_id_total",
		"Decryption attempts refused because the keyID matches no configured key")

	// IntegrityErrorsTotal counts hash mismatches detected during read,
	// replication, or background scrubbing.
	IntegrityErrorsTotal = counterVec("s3o_integrity_errors_total", "Content hash mismatches detected", "operation")

	// IntegrityUsageDeclinedTotal counts copies a scrub left unverified
	// because the backend had no egress headroom for them. A sweep that
	// declines is not finding corruption it cannot see; the copies are simply
	// unchecked, and stay unchecked until the budget allows.
	IntegrityUsageDeclinedTotal = counter("s3o_integrity_usage_declined_total",
		"Copies left unverified for lack of backend egress headroom")

	// IntegrityChecksTotal counts hash verifications performed.
	IntegrityChecksTotal = counterVec("s3o_integrity_checks_total", "Content hash verifications performed", "operation")

	// IntegrityOldestUnverifiedSeconds rises when the scrubber cannot keep
	// pace with the fleet, so it is the figure to alert on. A copy that has
	// never been verified counts from when it was written, so a fleet the
	// sweep has not reached is visible here rather than reading as zero.
	// Copies the sweep is not allowed to read are excluded and counted by
	// IntegrityDeferredCopies instead: they can never be stamped, so leaving
	// them in would make this climb by a day every day and never fall.
	IntegrityOldestUnverifiedSeconds = gauge("s3o_integrity_oldest_unverified_seconds",
		"How long the most overdue object copy has gone unverified")

	// IntegrityNeverVerifiedCopies stays non-zero on a fleet that is still
	// being written to, since new copies queue behind older data. Alert on it
	// climbing, not on it being above zero.
	IntegrityNeverVerifiedCopies = gauge("s3o_integrity_never_verified_copies",
		"Object copies with a content hash that have never been verified")

	// IntegrityDeferredCopies counts the copies the sweep cannot reach because
	// their backend is over its usage limit, or has used all the egress the
	// scrub reserve leaves the scrubber this month. Non-zero means the coverage
	// figures above describe only part of the fleet until the month resets.
	IntegrityDeferredCopies = gauge("s3o_integrity_deferred_copies",
		"Object copies the scrubber cannot reach because their backend is over its usage limit or scrub reserve")

	// EncryptionPlaintextCopies stays non-zero until an operator runs
	// encrypt-existing, since enabling encryption covers new writes only.
	EncryptionPlaintextCopies = gauge("s3o_encryption_plaintext_copies", "Object copies still stored unencrypted")

	// UnreadableCopies counts copies imported as encrypted with no key, which
	// stay until an operator purges them.
	UnreadableCopies = gauge("s3o_unreadable_copies", "Object copies encrypted with no key to decrypt them")

	// KeyRotationObjectsTotal counts objects processed during key rotation.
	KeyRotationObjectsTotal = counterVec("s3o_key_rotation_objects_total",
		"Total objects processed during key rotation", "status")

	// BulkRewriteUsageDeclinedTotal counts objects a bulk rewrite pass left
	// alone because the backend had no usage headroom for them, labelled by
	// the pass that declined. Separate from the per-pass skipped status: a
	// threshold skip is the feature working, this one means a fleet-wide pass
	// is being throttled by a monthly limit and will not finish its sweep.
	BulkRewriteUsageDeclinedTotal = counterVec("s3o_bulk_rewrite_usage_declined_total",
		"Objects skipped by a bulk rewrite pass for lack of backend usage headroom", "operation")

	// BulkRewriteCopyChangedTotal counts copies a bulk rewrite pass converted
	// and then could not record, because a client wrote the key while the pass
	// held it, labelled by the pass that lost the race. The client's write
	// stands and the pass's bytes are deleted; a non-zero value means a fleet
	// conversion is overlapping live traffic.
	BulkRewriteCopyChangedTotal = counterVec("s3o_bulk_rewrite_copy_changed_total",
		"Copies a bulk rewrite pass converted but could not record, because a client wrote the key first", "operation")

	// EncryptExistingObjectsTotal counts objects processed during encrypt-existing.
	EncryptExistingObjectsTotal = counterVec("s3o_encrypt_existing_objects_total",
		"Total objects processed during encrypt-existing operation", "status")

	// DecryptExistingObjectsTotal counts objects processed during decrypt-existing.
	DecryptExistingObjectsTotal = counterVec("s3o_decrypt_existing_objects_total",
		"Total objects processed during decrypt-existing operation", "status")
)

// EncryptionFlagMismatchTotal counts copies whose stored bytes disagreed with
// their row's encrypted flag, labelled by the component that noticed. Any
// non-zero value means at least one object cannot be safely read or hashed
// until its metadata is repaired.
var EncryptionFlagMismatchTotal = counterVec("s3o_encryption_flag_mismatch_total",
	"Copies whose stored bytes disagreed with their recorded encryption flag", "component")

// ImportClassifiedTotal counts objects brought under management by reconcile
// or sync, labelled by what their bytes turned out to be. A rising
// "unreadable" count means encrypted objects are being discovered whose key
// is gone, which no amount of retrying will fix.
var ImportClassifiedTotal = counterVec("s3o_import_classified_total",
	"Discovered objects imported, by what their bytes were classified as", "source", "decision")
