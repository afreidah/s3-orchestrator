// -------------------------------------------------------------------------------
// Core Object Tag Orchestration
//
// Author: Alex Freidah
//
// Engine-agnostic transactional logic for object_tags: validating a tag set,
// replacing one wholesale, reading it back, and clearing it wherever a key
// stops holding the object it held. Each operation composes TxAdapter calls
// inside a single transaction so the Postgres and SQLite paths share one
// implementation.
//
// Tags describe the object, not any one copy of it, so every operation here is
// keyed by object key alone and takes the key lock before touching a row.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"fmt"
)

// -------------------------------------------------------------------------
// LIMITS
// -------------------------------------------------------------------------

// AWS tag-set limits. Lengths are counted in UTF-16 code units rather than
// runes or bytes, because S3 represents tags internally in UTF-16 where a
// character occupies one or two positions.
const (
	MaxTagsPerObject  = 10
	MaxTagKeyLength   = 128
	MaxTagValueLength = 256
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Tag is one key/value label on an object. Both fields are case sensitive.
type Tag struct {
	Key   string
	Value string
}

// -------------------------------------------------------------------------
// VALIDATION
// -------------------------------------------------------------------------

// ValidateTags checks a proposed tag set against the AWS limits. Callers
// validate before opening a transaction so a rejected set costs no lock.
//
// An empty set is valid: it is how PutObjectTagging expresses a delete, which
// AWS defines as equivalent to DeleteObjectTagging.
func ValidateTags(tags []Tag) error {
	if len(tags) > MaxTagsPerObject {
		return fmt.Errorf("%w: %d tags supplied, limit is %d",
			ErrTooManyTags, len(tags), MaxTagsPerObject)
	}
	seen := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		if t.Key == "" {
			return ErrEmptyTagKey
		}
		if n := utf16Length(t.Key); n > MaxTagKeyLength {
			return fmt.Errorf("%w: key is %d UTF-16 units, limit is %d",
				ErrTagKeyTooLong, n, MaxTagKeyLength)
		}
		if n := utf16Length(t.Value); n > MaxTagValueLength {
			return fmt.Errorf("%w: value for key %q is %d UTF-16 units, limit is %d",
				ErrTagValueTooLong, t.Key, n, MaxTagValueLength)
		}
		if _, dup := seen[t.Key]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateTagKey, t.Key)
		}
		seen[t.Key] = struct{}{}
	}
	return nil
}

// utf16Length counts s in UTF-16 code units. Runes outside the basic
// multilingual plane encode as a surrogate pair and so cost two, which is why
// len([]rune(s)) is not the same measurement: a key of 128 emoji is 256 units
// and over the limit while the rune count reports 128.
func utf16Length(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
			continue
		}
		n++
	}
	return n
}

// -------------------------------------------------------------------------
// REPLACE TAGS
// -------------------------------------------------------------------------

// ReplaceObjectTags swaps an object's whole tag set for the supplied one.
//
// Replace rather than read-modify-write: the delete and the inserts share a
// transaction, so concurrent taggers are cleanly last-writer-wins instead of
// interleaving into a set neither of them asked for. An empty set leaves the
// object with no tags, which is what PutObjectTagging with an empty TagSet
// means.
func ReplaceObjectTags(ctx context.Context, runner Runner, key string, tags []Tag) error {
	if err := ValidateTags(tags); err != nil {
		return err
	}
	return runner.WithTx(ctx, func(ctx context.Context, tx TxAdapter) error {
		if err := tx.AcquireKeyLock(ctx, key); err != nil {
			return err
		}
		if err := requireObjectExists(ctx, tx, key); err != nil {
			return err
		}
		return replaceObjectTagsTx(ctx, tx, key, tags)
	})
}

// requireObjectExists reports ErrObjectNotFound when no copy of the key is
// stored, so a tagging call against a key that holds nothing is refused.
//
// Checked inside the transaction rather than by the caller beforehand: tag rows
// are only ever collected when a location row is removed, so a set written for
// a key with no locations is an orphan nothing sweeps. The key lock is already
// held here, and the copy read locks the rows, so a delete cannot land between
// the check and the write.
func requireObjectExists(ctx context.Context, tx TxAdapter, key string) error {
	existing, err := tx.GetExistingCopiesForUpdate(ctx, key)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		return ErrObjectNotFound
	}
	return nil
}

// replaceObjectTagsTx is the transactional body, shared with the write path so
// an object and its tags commit together rather than as two calls that can
// half-fail.
func replaceObjectTagsTx(ctx context.Context, tx TxAdapter, key string, tags []Tag) error {
	if err := tx.DeleteObjectTags(ctx, key); err != nil {
		return fmt.Errorf("clear existing tags: %w", err)
	}
	for _, t := range tags {
		if err := tx.InsertObjectTag(ctx, key, t.Key, t.Value); err != nil {
			return fmt.Errorf("insert tag %q: %w", t.Key, err)
		}
	}
	return nil
}

// -------------------------------------------------------------------------
// DELETE TAGS
// -------------------------------------------------------------------------

// DeleteObjectTags removes an object's whole tag set. Deleting a set that is
// already empty is a no-op rather than an error: AWS returns 204 for
// DeleteObjectTagging against an untagged object.
func DeleteObjectTags(ctx context.Context, runner Runner, key string) error {
	return runner.WithTx(ctx, func(ctx context.Context, tx TxAdapter) error {
		if err := tx.AcquireKeyLock(ctx, key); err != nil {
			return err
		}
		if err := requireObjectExists(ctx, tx, key); err != nil {
			return err
		}
		return tx.DeleteObjectTags(ctx, key)
	})
}

// -------------------------------------------------------------------------
// CASCADE
// -------------------------------------------------------------------------

// clearTagsForKey drops a key's tags from inside a transaction that is already
// removing or replacing the object at that key.
//
// Without object versioning there is no per-object table for a foreign key to
// point at, so object_tags is keyed on object_key alone and nothing cascades on
// its own. The object-scoped semantics AWS specifies only hold because every
// path that puts a new object at a key, or removes the last copy of one, calls
// this. Callers hold the key lock already.
func clearTagsForKey(ctx context.Context, tx TxAdapter, key string) error {
	if err := tx.DeleteObjectTags(ctx, key); err != nil {
		return fmt.Errorf("clear object tags: %w", err)
	}
	return nil
}

// clearTagsForKeys is the batch form, for the multi-key delete path.
func clearTagsForKeys(ctx context.Context, tx TxAdapter, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := tx.DeleteObjectTagsForKeys(ctx, keys); err != nil {
		return fmt.Errorf("clear object tags for keys: %w", err)
	}
	return nil
}
