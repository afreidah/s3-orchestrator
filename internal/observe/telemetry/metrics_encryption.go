// -------------------------------------------------------------------------------
// Metrics  -  Encryption, Integrity Verification
//
// Author: Alex Freidah
//
// Domain-scoped slice of the s3o_* Prometheus surface. Split out of the
// original 784-line metrics.go to keep each subsystem under ~150 lines.
// -------------------------------------------------------------------------------

package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// EncryptionOpsTotal and related package-level variables used by this package.
var (
	// --- Encryption metrics ---

	// EncryptionOpsTotal counts encryption operations by type.
	EncryptionOpsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_encryption_operations_total",
			Help: "Total encryption operations",
		},
		[]string{"op"},
	)

	// EncryptionErrorsTotal counts encryption errors by operation and type.
	EncryptionErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_encryption_errors_total",
			Help: "Total encryption errors",
		},
		[]string{"op", "error_type"},
	)

	// EncryptionUnknownKeyIDTotal counts decryption attempts where the keyID
	// was not found in the configured keys, triggering a primary key fallback.
	EncryptionUnknownKeyIDTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "s3o_encryption_unknown_key_id_total",
			Help: "Decryption attempts with unknown keyID (primary key fallback)",
		},
	)

	// --- Integrity verification metrics ---

	// IntegrityErrorsTotal counts hash mismatches detected during read,
	// replication, or background scrubbing.
	IntegrityErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_integrity_errors_total",
			Help: "Content hash mismatches detected",
		},
		[]string{"operation"},
	)

	// IntegrityChecksTotal counts hash verifications performed.
	IntegrityChecksTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_integrity_checks_total",
			Help: "Content hash verifications performed",
		},
		[]string{"operation"},
	)

	// IntegrityOldestUnverifiedSeconds rises when the scrubber cannot keep
	// pace with the fleet, so it is the figure to alert on.
	IntegrityOldestUnverifiedSeconds = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_integrity_oldest_unverified_seconds",
			Help: "Age of the least recently verified object copy",
		},
	)

	// IntegrityNeverVerifiedCopies stays non-zero on a fleet that is still
	// being written to, since new copies queue behind older data. Alert on it
	// climbing, not on it being above zero.
	IntegrityNeverVerifiedCopies = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_integrity_never_verified_copies",
			Help: "Object copies with a content hash that have never been verified",
		},
	)

	// EncryptionPlaintextCopies stays non-zero until an operator runs
	// encrypt-existing, since enabling encryption covers new writes only.
	EncryptionPlaintextCopies = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3o_encryption_plaintext_copies",
			Help: "Object copies still stored unencrypted",
		},
	)

	// KeyRotationObjectsTotal counts objects processed during key rotation.
	KeyRotationObjectsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_key_rotation_objects_total",
			Help: "Total objects processed during key rotation",
		},
		[]string{"status"},
	)

	// EncryptExistingObjectsTotal counts objects processed during encrypt-existing.
	EncryptExistingObjectsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_encrypt_existing_objects_total",
			Help: "Total objects processed during encrypt-existing operation",
		},
		[]string{"status"},
	)

	// DecryptExistingObjectsTotal counts objects processed during decrypt-existing.
	DecryptExistingObjectsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3o_decrypt_existing_objects_total",
			Help: "Total objects processed during decrypt-existing operation",
		},
		[]string{"status"},
	)
)

// EncryptionFlagMismatchTotal counts copies whose stored bytes disagreed with
// their row's encrypted flag, labelled by the component that noticed. Any
// non-zero value means at least one object cannot be safely read or hashed
// until its metadata is repaired.
var EncryptionFlagMismatchTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "s3o_encryption_flag_mismatch_total",
		Help: "Copies whose stored bytes disagreed with their recorded encryption flag",
	},
	[]string{"component"},
)

// ImportClassifiedTotal counts objects brought under management by reconcile
// or sync, labelled by what their bytes turned out to be. A rising
// "unreadable" count means encrypted objects are being discovered whose key
// is gone, which no amount of retrying will fix.
var ImportClassifiedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "s3o_import_classified_total",
		Help: "Discovered objects imported, by what their bytes were classified as",
	},
	[]string{"source", "decision"},
)
