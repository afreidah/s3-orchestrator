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

// -------------------------------------------------------------------------
// RECORD PART
// -------------------------------------------------------------------------

// partOutcome carries a part record's cleanup rows and, for a part whose intent
// was resolved while it uploaded, why it was refused. A refusal still commits,
// since it is what queues the bytes the upload wrote.
type partOutcome struct {
	cleanup []CleanupItem
	refusal error
}

// RecordPart records an uploaded part and clears its intent, returning the
// cleanup rows it queued for the bytes of a part it replaced. A part whose
// intent is gone was resolved by the pending reaper mid-upload; its bytes are
// queued and ErrIntentLost returned.
func RecordPart(ctx context.Context, runner Runner, p *RecordPartParams) ([]CleanupItem, error) {
	if p.PartNumber < 1 || p.PartNumber > 10000 {
		return nil, fmt.Errorf("invalid part number %d: must be between 1 and 10000", p.PartNumber)
	}
	out, err := WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) (partOutcome, error) {
		return recordPartTx(ctx, tx, p)
	})
	if err != nil {
		return nil, err
	}
	return out.cleanup, out.refusal
}

// recordPartTx is the transactional body of RecordPart. The lock on the part's
// fixed path serializes uploads of one part number, so the row a record
// replaces is the one whose bytes it queues. The upload row lock keeps the
// record from replacing a part a running completion is reading.
func recordPartTx(ctx context.Context, tx TxAdapter, p *RecordPartParams) (partOutcome, error) {
	partPath := PartPath(p.UploadID, p.PartNumber)
	if err := tx.AcquireKeyLock(ctx, partPath); err != nil {
		return partOutcome{}, err
	}
	backendName, err := lockUploadIdle(ctx, tx, p.UploadID, p.CompletionStaleBefore)
	if err != nil {
		return partOutcome{}, err
	}
	if backendName == "" {
		return partOutcome{}, ErrMultipartUploadNotFound
	}
	held, err := intentsHeld(ctx, tx, []string{p.Intent.IntentID})
	if err != nil {
		return partOutcome{}, err
	}
	if !held {
		cleanup, err := enqueueRemoved(ctx, tx, partPath, CleanupReasonSupersededIntent, []removedCopy{intentBytes(p.Intent, "")})
		return partOutcome{cleanup: cleanup, refusal: ErrIntentLost}, err
	}
	replaced, err := tx.UpsertPart(ctx, p)
	if err != nil {
		return partOutcome{}, err
	}
	if err := tx.DeletePending(ctx, p.Intent.IntentID); err != nil {
		return partOutcome{}, fmt.Errorf("delete recorded part intent: %w", err)
	}
	if replaced == nil {
		return partOutcome{}, nil
	}
	cleanup, err := enqueueRemoved(ctx, tx, partPath, CleanupReasonPartReplaced, []removedCopy{{
		BackendName: p.Intent.BackendName,
		StorageKey:  replaced.Path(p.UploadID),
		SizeBytes:   replaced.SizeBytes,
	}})
	return partOutcome{cleanup: cleanup}, err
}

// -------------------------------------------------------------------------
// DELETE MULTIPART UPLOAD
// -------------------------------------------------------------------------

// DeleteMultipartUpload removes an upload and its parts and queues every part's
// bytes under reason. An upload already gone queues nothing, and one whose
// completion renewed its mark after staleBefore is refused.
func DeleteMultipartUpload(ctx context.Context, runner Runner, uploadID, reason string, staleBefore time.Time) ([]CleanupItem, error) {
	return WithTxVal(ctx, runner, func(ctx context.Context, tx TxAdapter) ([]CleanupItem, error) {
		backendName, err := lockUploadIdle(ctx, tx, uploadID, staleBefore)
		if err != nil || backendName == "" {
			return nil, err
		}
		parts, err := tx.DeleteMultipartUpload(ctx, uploadID)
		if err != nil {
			return nil, err
		}
		items := make([]CleanupItem, 0, len(parts))
		for i := range parts {
			queued, err := enqueueRemoved(ctx, tx, PartPath(uploadID, parts[i].PartNumber), reason, []removedCopy{{
				BackendName: backendName,
				StorageKey:  parts[i].Path(uploadID),
				SizeBytes:   parts[i].SizeBytes,
			}})
			if err != nil {
				return nil, err
			}
			items = append(items, queued...)
		}
		return items, nil
	})
}

// lockUploadIdle locks the upload row and returns its backend, or an empty
// backend when the upload is gone. A completion that renewed its mark after
// staleBefore is still reading the parts, so the upload is refused with
// ErrCompletionInProgress; an older mark belongs to a completion that died.
func lockUploadIdle(ctx context.Context, tx TxAdapter, uploadID string, staleBefore time.Time) (string, error) {
	backendName, heartbeat, err := tx.LockMultipartUpload(ctx, uploadID)
	if err != nil {
		return "", err
	}
	if heartbeat.After(staleBefore) {
		return "", ErrCompletionInProgress
	}
	return backendName, nil
}
