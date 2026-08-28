// -------------------------------------------------------------------------------
// Core Object Tag Tests
//
// Author: Alex Freidah
//
// Covers tag-set validation against the AWS limits, replace semantics, and the
// clear-on-write and cascade rules that keep a key's tags tied to the object
// currently stored under it rather than to the path.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// -------------------------------------------------------------------------
// VALIDATION
// -------------------------------------------------------------------------

// TestValidateTags_Limits covers each rejection the AWS limits define, plus
// the sets that must be accepted: an empty one, and one exactly at the bound.
func TestValidateTags_Limits(t *testing.T) {
	t.Parallel()

	eleven := make([]Tag, 11)
	for i := range eleven {
		eleven[i] = Tag{Key: string(rune('a' + i)), Value: "v"}
	}
	ten := eleven[:10]

	tests := []struct {
		name string
		tags []Tag
		want error
	}{
		{"empty set is a delete, not an error", nil, nil},
		{"at the tag-count limit", ten, nil},
		{"over the tag-count limit", eleven, ErrTooManyTags},
		{"empty key", []Tag{{Key: "", Value: "v"}}, ErrEmptyTagKey},
		{"key at limit", []Tag{{Key: strings.Repeat("k", 128), Value: "v"}}, nil},
		{"key over limit", []Tag{{Key: strings.Repeat("k", 129), Value: "v"}}, ErrTagKeyTooLong},
		{"value at limit", []Tag{{Key: "k", Value: strings.Repeat("v", 256)}}, nil},
		{"value over limit", []Tag{{Key: "k", Value: strings.Repeat("v", 257)}}, ErrTagValueTooLong},
		{"empty value is allowed", []Tag{{Key: "k", Value: ""}}, nil},
		{"duplicate key", []Tag{{Key: "k", Value: "1"}, {Key: "k", Value: "2"}}, ErrDuplicateTagKey},
		{"keys differing only in case are not duplicates", []Tag{{Key: "k", Value: "1"}, {Key: "K", Value: "2"}}, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateTags(tc.tags)
			if !errors.Is(err, tc.want) {
				t.Errorf("ValidateTags() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestValidateTags_UTF16Lengths verifies lengths are counted in UTF-16 code
// units. An astral rune costs two units, so a key of 64 emoji is 128 units and
// sits exactly at the limit while one more rune goes over - a rune or byte
// count would put both on the wrong side.
func TestValidateTags_UTF16Lengths(t *testing.T) {
	t.Parallel()

	// U+1F600, outside the basic multilingual plane: one rune, two units.
	const emoji = "\U0001F600"

	atLimit := []Tag{{Key: strings.Repeat(emoji, 64), Value: "v"}}
	if err := ValidateTags(atLimit); err != nil {
		t.Errorf("64 astral runes is 128 UTF-16 units and should be accepted, got %v", err)
	}

	overLimit := []Tag{{Key: strings.Repeat(emoji, 65), Value: "v"}}
	if err := ValidateTags(overLimit); !errors.Is(err, ErrTagKeyTooLong) {
		t.Errorf("65 astral runes is 130 UTF-16 units and should be rejected, got %v", err)
	}

	// The same rune count in BMP characters stays well inside the limit,
	// which is what makes the distinction observable.
	if err := ValidateTags([]Tag{{Key: strings.Repeat("a", 65), Value: "v"}}); err != nil {
		t.Errorf("65 BMP runes is 65 units and should be accepted, got %v", err)
	}
}

// TestUTF16Length covers the counter directly across the plane boundary.
func TestUTF16Length(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"é", 1},            // Latin small e with acute, BMP
		{"￿", 1},            // last BMP code point
		{"\U00010000", 2},   // first astral code point
		{"a\U0001F600b", 4}, // mixed
	}
	for _, tc := range tests {
		if got := utf16Length(tc.in); got != tc.want {
			t.Errorf("utf16Length(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// -------------------------------------------------------------------------
// REPLACE
// -------------------------------------------------------------------------

// TestReplaceObjectTags_ClearsThenInserts verifies replace is a delete
// followed by inserts rather than a read-modify-write, which is what makes
// concurrent taggers last-writer-wins instead of merging.
func TestReplaceObjectTags_ClearsThenInserts(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{existingCopies: storedCopy()}
	tags := []Tag{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}}

	if err := ReplaceObjectTags(context.Background(), &stubRunner{tx: stub}, "k", tags); err != nil {
		t.Fatalf("ReplaceObjectTags() error = %v", err)
	}
	if len(stub.tagsCleared) != 1 || stub.tagsCleared[0] != "k" {
		t.Errorf("expected exactly one clear of %q, got %v", "k", stub.tagsCleared)
	}
	if len(stub.tagsInserted) != 2 {
		t.Fatalf("expected 2 inserts, got %d: %v", len(stub.tagsInserted), stub.tagsInserted)
	}
}

// TestReplaceObjectTags_RejectsBeforeTransaction verifies an invalid set is
// refused without touching the adapter, so a bad request costs no lock.
func TestReplaceObjectTags_RejectsBeforeTransaction(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{existingCopies: storedCopy()}
	bad := []Tag{{Key: "k", Value: "1"}, {Key: "k", Value: "2"}}

	if err := ReplaceObjectTags(context.Background(), &stubRunner{tx: stub}, "k", bad); !errors.Is(err, ErrDuplicateTagKey) {
		t.Fatalf("ReplaceObjectTags() error = %v, want ErrDuplicateTagKey", err)
	}
	if len(stub.tagsCleared) != 0 || len(stub.tagsInserted) != 0 {
		t.Errorf("adapter touched on a rejected set: cleared=%v inserted=%v",
			stub.tagsCleared, stub.tagsInserted)
	}
}

// TestReplaceObjectTags_LockError verifies a failure to take the key lock
// surfaces before anything is written. Proceeding without it would let a
// concurrent delete strand the tags this call is about to insert.
func TestReplaceObjectTags_LockError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("lock failed")
	stub := &quotaTxStub{keyLockErr: sentinel, existingCopies: storedCopy()}

	err := ReplaceObjectTags(context.Background(), &stubRunner{tx: stub}, "k", []Tag{{Key: "a", Value: "1"}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ReplaceObjectTags() error = %v, want the lock error", err)
	}
	if len(stub.tagsCleared) != 0 || len(stub.tagsInserted) != 0 {
		t.Errorf("wrote without the lock: cleared=%v inserted=%v", stub.tagsCleared, stub.tagsInserted)
	}
}

// TestReplaceObjectTags_ClearError verifies a failed clear aborts the replace
// rather than inserting on top of a set that is still there, which would leave
// the union of the old and new sets.
func TestReplaceObjectTags_ClearError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("clear failed")
	stub := &quotaTxStub{tagClearErr: sentinel, existingCopies: storedCopy()}

	err := ReplaceObjectTags(context.Background(), &stubRunner{tx: stub}, "k", []Tag{{Key: "a", Value: "1"}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ReplaceObjectTags() error = %v, want the clear error", err)
	}
	if len(stub.tagsInserted) != 0 {
		t.Errorf("inserted despite the clear failing: %v", stub.tagsInserted)
	}
}

// TestReplaceObjectTags_InsertError verifies a failed insert surfaces so the
// transaction rolls back, rather than committing a partially written set.
func TestReplaceObjectTags_InsertError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("insert failed")
	stub := &quotaTxStub{tagInsertErr: sentinel, existingCopies: storedCopy()}

	err := ReplaceObjectTags(context.Background(), &stubRunner{tx: stub},
		"k", []Tag{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ReplaceObjectTags() error = %v, want the insert error", err)
	}
}

// TestReplaceObjectTags_NotFound verifies tagging a key that holds nothing is
// refused. Tag rows are only ever collected when a location row is removed, so
// a set written against a key with no locations is an orphan nothing sweeps.
func TestReplaceObjectTags_NotFound(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{}

	err := ReplaceObjectTags(context.Background(), &stubRunner{tx: stub}, "gone", []Tag{{Key: "a", Value: "1"}})
	if !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("ReplaceObjectTags() error = %v, want ErrObjectNotFound", err)
	}
	if len(stub.tagsCleared) != 0 || len(stub.tagsInserted) != 0 {
		t.Errorf("wrote tags for a key holding nothing: cleared=%v inserted=%v",
			stub.tagsCleared, stub.tagsInserted)
	}
}

// TestReplaceObjectTags_ExistenceCheckError verifies a failure to establish
// whether the object exists aborts the write, rather than being read as
// absent (which would 404) or ignored (which would orphan the rows).
func TestReplaceObjectTags_ExistenceCheckError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("copy read failed")
	stub := &quotaTxStub{existingErr: sentinel}

	err := ReplaceObjectTags(context.Background(), &stubRunner{tx: stub}, "k", []Tag{{Key: "a", Value: "1"}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ReplaceObjectTags() error = %v, want the read error", err)
	}
	if len(stub.tagsCleared) != 0 || len(stub.tagsInserted) != 0 {
		t.Errorf("wrote despite an unresolved existence check: cleared=%v inserted=%v",
			stub.tagsCleared, stub.tagsInserted)
	}
}

// TestDeleteObjectTags_NotFound verifies the delete refuses a key that holds
// nothing, rather than reporting success for an object that is not there.
func TestDeleteObjectTags_NotFound(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{}

	if err := DeleteObjectTags(context.Background(), &stubRunner{tx: stub}, "gone"); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("DeleteObjectTags() error = %v, want ErrObjectNotFound", err)
	}
	if len(stub.tagsCleared) != 0 {
		t.Errorf("cleared tags for a key holding nothing: %v", stub.tagsCleared)
	}
}

// TestDeleteObjectTags_LockError verifies the delete takes the key lock and
// surfaces a failure to get it.
func TestDeleteObjectTags_LockError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("lock failed")
	stub := &quotaTxStub{keyLockErr: sentinel, existingCopies: storedCopy()}

	if err := DeleteObjectTags(context.Background(), &stubRunner{tx: stub}, "k"); !errors.Is(err, sentinel) {
		t.Fatalf("DeleteObjectTags() error = %v, want the lock error", err)
	}
	if len(stub.tagsCleared) != 0 {
		t.Errorf("cleared without the lock: %v", stub.tagsCleared)
	}
}

// TestDeleteObjectTags_ClearsSet covers the success path: the set is removed
// and the call reports no error even when there was nothing to remove.
func TestDeleteObjectTags_ClearsSet(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{existingCopies: storedCopy()}

	if err := DeleteObjectTags(context.Background(), &stubRunner{tx: stub}, "k"); err != nil {
		t.Fatalf("DeleteObjectTags() error = %v", err)
	}
	if len(stub.tagsCleared) != 1 || stub.tagsCleared[0] != "k" {
		t.Errorf("expected a clear of %q, got %v", "k", stub.tagsCleared)
	}
}

// TestClearTagsForKeys_EmptyInput verifies the batch clear short-circuits on
// an empty list rather than issuing a statement matching nothing.
func TestClearTagsForKeys_EmptyInput(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{}

	if err := clearTagsForKeys(context.Background(), stub, nil); err != nil {
		t.Fatalf("clearTagsForKeys() error = %v", err)
	}
	if len(stub.tagKeysCleared) != 0 {
		t.Errorf("expected no statement for an empty list, got %v", stub.tagKeysCleared)
	}
}

// TestClearTagsForKeys_Error verifies a batch clear failure surfaces wrapped
// rather than being dropped, so the enclosing delete rolls back.
func TestClearTagsForKeys_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("batch clear failed")
	stub := &quotaTxStub{tagClearErr: sentinel}

	if err := clearTagsForKeys(context.Background(), stub, []string{"a"}); !errors.Is(err, sentinel) {
		t.Fatalf("clearTagsForKeys() error = %v, want the clear error", err)
	}
}

// TestReplaceObjectTags_EmptySetClears verifies an empty TagSet leaves the
// object with no tags, matching DeleteObjectTagging as AWS specifies.
func TestReplaceObjectTags_EmptySetClears(t *testing.T) {
	t.Parallel()
	stub := &quotaTxStub{existingCopies: storedCopy()}

	if err := ReplaceObjectTags(context.Background(), &stubRunner{tx: stub}, "k", nil); err != nil {
		t.Fatalf("ReplaceObjectTags() error = %v", err)
	}
	if len(stub.tagsCleared) != 1 {
		t.Errorf("expected the set to be cleared, got %v", stub.tagsCleared)
	}
	if len(stub.tagsInserted) != 0 {
		t.Errorf("expected no inserts for an empty set, got %v", stub.tagsInserted)
	}
}
