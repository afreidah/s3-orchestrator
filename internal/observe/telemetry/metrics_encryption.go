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
