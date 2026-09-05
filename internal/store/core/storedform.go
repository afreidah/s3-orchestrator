// -------------------------------------------------------------------------------
// Stored-Form Rewrites - Compression and Encryption Passes
//
// Author: Alex Freidah
//
// The three operations that rewrite what a copy is stored as: compressed,
// encrypted, or decrypted back to plaintext. Each one changes the size of the
// bytes on the backend, so each one has to move that backend's byte counter by
// the difference in the same transaction that records the new form - otherwise
// bytes_used drifts from SUM(object_locations.size_bytes) and write routing
// starts refusing writes that would fit.
//
// The engines contribute the statements; the delta arithmetic and the rule that
// a zero delta touches nothing are stated once, here.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"fmt"
)

// MarkObjectCompressed records the new stored form of a rewritten copy and
// moves the backend's quota by the difference between what the copy occupied
// before and what it occupies now.
//
// The envelope columns are rewritten too: re-encrypting an object mints a new
// base nonce and wrapped key, so leaving the old ones would describe bytes
// nothing can decrypt.
func MarkObjectCompressed(ctx context.Context, runner Runner, u *CompressedUpdate, previousSize int64) error {
	return runner.WithTx(ctx, func(ctx context.Context, tx TxAdapter) error {
		if err := tx.UpdateCompressedForm(ctx, u); err != nil {
			return fmt.Errorf("mark compressed: %w", err)
		}
		return resizeBackendUsage(ctx, tx, u.BackendName, u.ObjectKey, u.SizeBytes-previousSize, "compression")
	})
}

// MarkObjectEncrypted records that a copy now holds an encryption envelope,
// storing the wrapped DEK and the plaintext size it was built from, and
// charges the backend for the ciphertext's extra bytes.
func MarkObjectEncrypted(ctx context.Context, runner Runner, u *EncryptedUpdate) error {
	return runner.WithTx(ctx, func(ctx context.Context, tx TxAdapter) error {
		if err := tx.MarkCopyEncrypted(ctx, u); err != nil {
			return fmt.Errorf("mark encrypted: %w", err)
		}
		return resizeBackendUsage(ctx, tx, u.BackendName, u.ObjectKey, u.CiphertextSize-u.PlaintextSize, "encryption")
	})
}

// MarkObjectDecrypted records that a copy is plaintext again, clearing the
// envelope columns and crediting the backend the bytes the envelope cost.
//
// The size the copy occupied is read inside the transaction before it is
// overwritten, because that read is the only place the delta can come from:
// the caller knows the plaintext size it wrote and not the ciphertext size it
// replaced.
func MarkObjectDecrypted(ctx context.Context, runner Runner, objectKey, backendName string, plaintextSize int64) error {
	return runner.WithTx(ctx, func(ctx context.Context, tx TxAdapter) error {
		currentSize, err := tx.GetCopySizeBytes(ctx, objectKey, backendName)
		if err != nil {
			return fmt.Errorf("read current size: %w", err)
		}
		if err := tx.MarkCopyDecrypted(ctx, objectKey, backendName, plaintextSize); err != nil {
			return fmt.Errorf("mark decrypted: %w", err)
		}
		return resizeBackendUsage(ctx, tx, backendName, objectKey, plaintextSize-currentSize, "decryption")
	})
}

// resizeBackendUsage moves a backend's byte counter by what a rewrite changed
// about one copy. A rewrite that changed no bytes leaves the counter alone
// rather than writing the same value back.
//
// The adjustment carries no bytes_limit guard, and the engines clamp it at
// zero: the bytes on disk are reality and the counter follows them whether or
// not the limit would otherwise be exceeded, while a stale size must never
// drive the counter negative and over-admit every later write.
func resizeBackendUsage(ctx context.Context, tx TxAdapter, backendName, key string, delta int64, pass string) error {
	if delta == 0 {
		return nil
	}
	if err := tx.AdjustQuotaStripe(ctx, backendName, StripeFor(key), delta); err != nil {
		return fmt.Errorf("adjust quota for %s: %w", pass, err)
	}
	return nil
}
