// -------------------------------------------------------------------------------
// Core Multipart Orchestration Tests
//
// Author: Alex Freidah
//
// Engine-agnostic coverage for RecordPart and DeleteMultipartUpload. Recording
// a part locks the part's fixed path, confirms its intent is still held, and
// queues the bytes of any part it replaced; a lost intent queues the upload's
// own bytes and refuses. Removing an upload queues every part's bytes at the
// path each one occupies.
// -------------------------------------------------------------------------------

package core

import (
	"context"
	"errors"
	"slices"
	"testing"
)

const (
	partUploadID = "up-1"
	partBackend  = "b1"
	partIntentID = "i-part"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// partTxStub drives the multipart transactions: it decides whether the part's
// intent is held, which part an upsert replaced, and what an upload removal
// finds, and records the locks, upserts and intent deletes it sees.
type partTxStub struct {
	noopTxAdapter

	lost          bool
	claimErr      error
	lockErr       error
	replaced      *MultipartPart
	upsertErr     error
	deletePendErr error

	uploadBackend string
	uploadParts   []MultipartPart
	uploadErr     error

	locks          []string
	upserted       []RecordPartParams
	deletedIntents []string
}

func (s *partTxStub) AcquireKeyLock(_ context.Context, key string) error {
	s.locks = append(s.locks, key)
	return s.lockErr
}

func (s *partTxStub) ClaimPending(context.Context, string) (bool, error) {
	return !s.lost, s.claimErr
}

func (s *partTxStub) DeletePending(_ context.Context, intentID string) error {
	if s.deletePendErr != nil {
		return s.deletePendErr
	}
	s.deletedIntents = append(s.deletedIntents, intentID)
	return nil
}

func (s *partTxStub) UpsertPart(_ context.Context, p *RecordPartParams) (*MultipartPart, error) {
	if s.upsertErr != nil {
		return nil, s.upsertErr
	}
	s.upserted = append(s.upserted, *p)
	return s.replaced, nil
}

func (s *partTxStub) DeleteMultipartUpload(context.Context, string) (string, []MultipartPart, error) {
	return s.uploadBackend, s.uploadParts, s.uploadErr
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// partParams is a record of part 2 of the test upload, written under its own
// intent at its own path.
func partParams() *RecordPartParams {
	return &RecordPartParams{
		UploadID:   partUploadID,
		PartNumber: 2,
		ETag:       "etag",
		SizeBytes:  500,
		Intent: &PendingObject{
			IntentID:    partIntentID,
			ObjectKey:   PartPath(partUploadID, 2),
			BackendName: partBackend,
			StorageKey:  PartPath(partUploadID, 2) + "!new",
			SizeBytes:   500,
		},
	}
}

// -------------------------------------------------------------------------
// RECORD PART
// -------------------------------------------------------------------------

// TestRecordPart_RejectsPartNumberOutOfRange verifies a part number outside
// what S3 allows is refused before any transaction runs.
func TestRecordPart_RejectsPartNumberOutOfRange(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 10001} {
		stub := &partTxStub{}
		p := partParams()
		p.PartNumber = n
		if _, err := RecordPart(context.Background(), &stubRunner{tx: stub}, p); err == nil {
			t.Errorf("part %d: expected an error", n)
		}
		if len(stub.locks) != 0 {
			t.Errorf("part %d: transaction ran: %v", n, stub.locks)
		}
	}
}

// TestRecordPart_FreshPartQueuesNothing verifies a part number recorded for
// the first time takes the part path's lock, records the part, clears its
// intent, and queues nothing.
func TestRecordPart_FreshPartQueuesNothing(t *testing.T) {
	t.Parallel()
	stub := &partTxStub{}
	cleanup, err := RecordPart(context.Background(), &stubRunner{tx: stub}, partParams())
	if err != nil {
		t.Fatalf("RecordPart: %v", err)
	}
	if cleanup != nil {
		t.Errorf("cleanup = %+v, want none", cleanup)
	}
	if !slices.Equal(stub.locks, []string{PartPath(partUploadID, 2)}) {
		t.Errorf("locks = %v, want the part's fixed path", stub.locks)
	}
	if len(stub.upserted) != 1 || !slices.Equal(stub.deletedIntents, []string{partIntentID}) {
		t.Errorf("upserted=%d intents=%v, want the part recorded and its intent cleared", len(stub.upserted), stub.deletedIntents)
	}
}

// TestRecordPart_QueuesTheReplacedPart verifies a re-upload of a part number
// queues the bytes of the part it replaced, at the path that part occupies,
// whether it carried its own storage key or sat at the fixed path.
func TestRecordPart_QueuesTheReplacedPart(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		replaced MultipartPart
		wantPath string
	}{
		"own path":   {MultipartPart{PartNumber: 2, SizeBytes: 300, StorageKey: "__multipart/up-1/2!old"}, "__multipart/up-1/2!old"},
		"fixed path": {MultipartPart{PartNumber: 2, SizeBytes: 300}, PartPath(partUploadID, 2)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			replaced := tc.replaced
			stub := &partTxStub{replaced: &replaced}
			queuer := &queueRecorder{TxAdapter: stub}
			cleanup, err := RecordPart(context.Background(), &stubRunner{tx: queuer}, partParams())
			if err != nil {
				t.Fatalf("RecordPart: %v", err)
			}
			want := []CleanupItem{{
				ID: 1, BackendName: partBackend, ObjectKey: PartPath(partUploadID, 2),
				StorageKey: tc.wantPath, Reason: CleanupReasonPartReplaced, SizeBytes: 300,
			}}
			if !slices.Equal(cleanup, want) {
				t.Errorf("cleanup = %+v, want %+v", cleanup, want)
			}
		})
	}
}

// TestRecordPart_LostIntentQueuesTheUpload verifies a part whose intent the
// reaper resolved mid-upload is not recorded: its own bytes are queued as a
// superseded intent's, and ErrIntentLost is returned with them.
func TestRecordPart_LostIntentQueuesTheUpload(t *testing.T) {
	t.Parallel()
	stub := &partTxStub{lost: true}
	p := partParams()
	cleanup, err := RecordPart(context.Background(), &stubRunner{tx: stub}, p)
	if !errors.Is(err, ErrIntentLost) {
		t.Fatalf("err = %v, want ErrIntentLost", err)
	}
	if len(stub.upserted) != 0 || len(stub.deletedIntents) != 0 {
		t.Errorf("lost part was recorded: upserted=%d intents=%v", len(stub.upserted), stub.deletedIntents)
	}
	want := []CleanupItem{{
		BackendName: partBackend, ObjectKey: PartPath(partUploadID, 2),
		StorageKey: p.Intent.StorageKey, Reason: CleanupReasonSupersededIntent, SizeBytes: 500,
	}}
	if !slices.Equal(cleanup, want) {
		t.Errorf("cleanup = %+v, want %+v", cleanup, want)
	}
}

// TestRecordPart_FailuresRollBack verifies each step's failure surfaces with
// no cleanup rows, since the transaction rolls back.
func TestRecordPart_FailuresRollBack(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("step failed")
	replaced := &MultipartPart{PartNumber: 2, SizeBytes: 300}
	cases := map[string]TxAdapter{
		"key lock":      &partTxStub{lockErr: sentinel},
		"claim":         &partTxStub{claimErr: sentinel},
		"upsert":        &partTxStub{upsertErr: sentinel},
		"intent delete": &partTxStub{deletePendErr: sentinel},
		"queue":         &queueRecorder{TxAdapter: &partTxStub{replaced: replaced}, err: sentinel},
		"lost queue":    &queueRecorder{TxAdapter: &partTxStub{lost: true}, err: sentinel},
	}
	for name, tx := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cleanup, err := RecordPart(context.Background(), &stubRunner{tx: tx}, partParams())
			if !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want the %s failure", err, name)
			}
			if cleanup != nil {
				t.Errorf("cleanup = %+v, want nothing on a rollback", cleanup)
			}
		})
	}
}

// -------------------------------------------------------------------------
// DELETE MULTIPART UPLOAD
// -------------------------------------------------------------------------

// TestDeleteMultipartUpload_QueuesEveryPart verifies removing an upload queues
// each part's bytes on the upload's backend at the path that part occupies,
// labelled with the caller's reason.
func TestDeleteMultipartUpload_QueuesEveryPart(t *testing.T) {
	t.Parallel()
	stub := &partTxStub{
		uploadBackend: partBackend,
		uploadParts: []MultipartPart{
			{PartNumber: 1, SizeBytes: 100},
			{PartNumber: 2, SizeBytes: 200, StorageKey: "__multipart/up-1/2!w"},
		},
	}
	cleanup, err := DeleteMultipartUpload(context.Background(), &stubRunner{tx: stub}, partUploadID, "abort")
	if err != nil {
		t.Fatalf("DeleteMultipartUpload: %v", err)
	}
	want := []CleanupItem{
		{BackendName: partBackend, ObjectKey: PartPath(partUploadID, 1), StorageKey: PartPath(partUploadID, 1), Reason: "abort", SizeBytes: 100},
		{BackendName: partBackend, ObjectKey: PartPath(partUploadID, 2), StorageKey: "__multipart/up-1/2!w", Reason: "abort", SizeBytes: 200},
	}
	if !slices.Equal(cleanup, want) {
		t.Errorf("cleanup = %+v, want %+v", cleanup, want)
	}
}

// TestDeleteMultipartUpload_GoneQueuesNothing verifies an upload already
// removed queues nothing and is not an error.
func TestDeleteMultipartUpload_GoneQueuesNothing(t *testing.T) {
	t.Parallel()
	cleanup, err := DeleteMultipartUpload(context.Background(), &stubRunner{tx: &partTxStub{}}, partUploadID, "abort")
	if err != nil || len(cleanup) != 0 {
		t.Errorf("cleanup = %+v err = %v, want nothing", cleanup, err)
	}
}

// TestDeleteMultipartUpload_FailuresRollBack verifies a failed removal or a
// part that cannot be queued surfaces with no cleanup rows.
func TestDeleteMultipartUpload_FailuresRollBack(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("step failed")
	parts := []MultipartPart{{PartNumber: 1, SizeBytes: 100}}
	cases := map[string]TxAdapter{
		"remove": &partTxStub{uploadErr: sentinel},
		"queue":  &queueRecorder{TxAdapter: &partTxStub{uploadBackend: partBackend, uploadParts: parts}, err: sentinel},
	}
	for name, tx := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cleanup, err := DeleteMultipartUpload(context.Background(), &stubRunner{tx: tx}, partUploadID, "abort")
			if !errors.Is(err, sentinel) || cleanup != nil {
				t.Errorf("cleanup = %+v err = %v, want the %s failure and nothing queued", cleanup, err, name)
			}
		})
	}
}
