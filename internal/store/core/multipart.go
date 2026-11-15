// -------------------------------------------------------------------------------
// Core Multipart Orchestration
//
// Author: Alex Freidah
//
// Engine-agnostic transactional logic for multipart parts. Each part upload
// writes its own path under an intent claimed on the upload's backend, the
// same way every other write claims its target. Recording the part clears the
// intent and queues the bytes of any part the upload replaced, and removing an
// upload queues every part's bytes, so no part is left on a backend without a
// row naming it.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"fmt"
	"time"
)

// MinPartNumber and MaxPartNumber are the part numbers S3 allows. The maximum
// also bounds how many parts one upload can hold.
const (
	MinPartNumber = 1
	MaxPartNumber = 10000
)

// UploadLock is how a transaction holds an upload row. Part records share it,
// so parts of one upload record together; an abort takes it exclusive. Either
// way a completion's mark waits for the holder.
type UploadLock int

// UploadShared is held by part records and UploadExclusive by an abort.
const (
	UploadShared UploadLock = iota
	UploadExclusive
)

// ValidPartNumber reports whether n is a part number S3 allows. Every layer
// that accepts a part number checks it here.
func ValidPartNumber(n int) bool {
	return n >= MinPartNumber && n <= MaxPartNumber
}

// -------------------------------------------------------------------------
// RECORD PART
// -------------------------------------------------------------------------

// RecordPart records an uploaded part and clears its intent, returning the
// cleanup rows it queued for the bytes of a part it replaced. A part whose
// intent is gone was resolved by the pending reaper mid-upload; its bytes are
// queued and ErrIntentLost returned.
func (o TxOps) RecordPart(ctx context.Context, p *RecordPartParams) ([]CleanupItem, error) {
	if !ValidPartNumber(p.PartNumber) {
		return nil, fmt.Errorf("invalid part number %d: must be between %d and %d", p.PartNumber, MinPartNumber, MaxPartNumber)
	}
	out, err := WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) (outcome[struct{}], error) {
		return recordPartTx(ctx, tx, p)
	})
	if err != nil {
		return nil, err
	}
	return out.cleanup, out.refusal
}

// recordPartTx is the transactional body of RecordPart. The lock on the part's
// fixed path serializes uploads of one part number, so the row a record
// replaces is the one whose bytes it queues. The shared upload row lock keeps
// the record from replacing a part a running completion is reading.
func recordPartTx(ctx context.Context, tx TxAdapter, p *RecordPartParams) (outcome[struct{}], error) {
	partPath := PartPath(p.UploadID, p.PartNumber)
	if err := tx.AcquireKeyLock(ctx, partPath); err != nil {
		return outcome[struct{}]{}, err
	}
	backendName, err := lockUploadIdle(ctx, tx, p.UploadID, UploadShared, p.CompletionStaleBefore)
	if err != nil {
		return outcome[struct{}]{}, err
	}
	if backendName == "" {
		return outcome[struct{}]{}, ErrMultipartUploadNotFound
	}
	held, err := intentsHeld(ctx, tx, []string{p.Intent.IntentID})
	if err != nil {
		return outcome[struct{}]{}, err
	}
	e := newLedgerEdit(tx)
	intent := p.Intent
	if !held {
		e.queue(partPath, CleanupReasonSupersededIntent, intent.BackendName, intent.StorageKey, intent.SizeBytes)
		return finish(ctx, e, struct{}{}, ErrIntentLost)
	}
	replaced, err := tx.UpsertPart(ctx, p)
	if err != nil {
		return outcome[struct{}]{}, err
	}
	if err := tx.DeletePending(ctx, intent.IntentID); err != nil {
		return outcome[struct{}]{}, fmt.Errorf("delete recorded part intent: %w", err)
	}
	if replaced != nil {
		e.queue(partPath, CleanupReasonPartReplaced, intent.BackendName, replaced.Path(p.UploadID), replaced.SizeBytes)
	}
	return finish(ctx, e, struct{}{}, nil)
}

// -------------------------------------------------------------------------
// DELETE MULTIPART UPLOAD
// -------------------------------------------------------------------------

// DeleteMultipartUpload removes an upload and its parts and queues every part's
// bytes under reason. An upload already gone queues nothing, and one whose
// completion renewed its mark after staleBefore is refused.
func (o TxOps) DeleteMultipartUpload(ctx context.Context, uploadID, reason string, staleBefore time.Time) ([]CleanupItem, error) {
	return WithTxVal(ctx, o.runner, func(ctx context.Context, tx TxAdapter) ([]CleanupItem, error) {
		backendName, err := lockUploadIdle(ctx, tx, uploadID, UploadExclusive, staleBefore)
		if err != nil || backendName == "" {
			return nil, err
		}
		parts, err := tx.DeleteMultipartUpload(ctx, uploadID)
		if err != nil {
			return nil, err
		}
		e := newLedgerEdit(tx)
		for i := range parts {
			e.queue(PartPath(uploadID, parts[i].PartNumber), reason, backendName, parts[i].Path(uploadID), parts[i].SizeBytes)
		}
		return e.commit(ctx)
	})
}

// lockUploadIdle locks the upload row in mode and returns its backend, or an
// empty backend when the upload is gone. A completion that renewed its mark
// after staleBefore is still reading the parts, so the upload is refused with
// ErrCompletionInProgress; an older mark belongs to a completion that died.
func lockUploadIdle(ctx context.Context, tx TxAdapter, uploadID string, mode UploadLock, staleBefore time.Time) (string, error) {
	backendName, heartbeat, err := tx.LockMultipartUpload(ctx, uploadID, mode)
	if err != nil {
		return "", err
	}
	if heartbeat.After(staleBefore) {
		return "", ErrCompletionInProgress
	}
	return backendName, nil
}
