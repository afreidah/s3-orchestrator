// -------------------------------------------------------------------------------
// Completion Mark Tests
//
// Author: Alex Freidah
//
// CompleteMultipartUpload holds the upload's completion mark while it
// assembles, renews it on a ticker, removes the upload after the commit while
// still holding it, and clears it when the assembly returns. Part uploads and
// aborts pass the stale cutoff so the store refuses them while the mark is
// fresh.
// -------------------------------------------------------------------------------

package multipart

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// markUpload is the upload row the completion mark tests run against, on the
// one backend their fleets hold.
func markUpload() *core.MultipartUpload {
	return &core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1", ContentType: "application/zip"}
}

// assertStaleCutoff fails the test unless got is the completion grace before
// a moment between before and the end of the call under test.
func assertStaleCutoff(t *testing.T, got, before time.Time) {
	t.Helper()
	if got.Before(before.Add(-completionGrace)) || got.After(time.Now().Add(-completionGrace)) {
		t.Errorf("stale cutoff = %v, want the completion grace before the call", got)
	}
}

// -------------------------------------------------------------------------
// COMPLETE
// -------------------------------------------------------------------------

// TestCompleteMultipartUpload_RemovesUploadWhileHoldingMark asserts a
// successful completion removes the upload before it clears the mark, so a
// retried completion cannot take the mark and assemble the parts again.
func TestCompleteMultipartUpload_RemovesUploadWhileHoldingMark(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	store, c := completeStoreSetup(t, markUpload(), putParts(t, be, 2), nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1, 2)); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	want := []string{markBegin, markDelete + reasonCompletePart, markEnd}
	if got := c.events(); !slices.Equal(got, want) {
		t.Errorf("mark events = %v, want %v", got, want)
	}
}

// TestCompleteMultipartUpload_GoneUploadIsNotFound asserts a mark that cannot
// be taken because the upload was removed since the fetch reports NoSuchUpload
// rather than a completion in progress.
func TestCompleteMultipartUpload_GoneUploadIsNotFound(t *testing.T) {
	t.Parallel()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetMultipartUpload(gomock.Any(), "upload-1").Return(markUpload(), nil)
	store.EXPECT().GetMultipartUpload(gomock.Any(), "upload-1").Return(nil, core.ErrMultipartUploadNotFound)
	store.EXPECT().BeginMultipartCompletion(gomock.Any(), "upload-1", gomock.Any()).Return(false, nil)
	storetest.Permissive(store)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": backendtest.NewInMemory()}, nil)

	_, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1))
	if !errors.Is(err, core.ErrMultipartUploadNotFound) {
		t.Fatalf("err = %v, want ErrMultipartUploadNotFound", err)
	}
}

// TestCompleteMultipartUpload_AssemblyFailureEndsMark asserts a failed
// assembly clears the mark, so the client can retry and the upload can be
// aborted, and leaves the upload in place.
func TestCompleteMultipartUpload_AssemblyFailureEndsMark(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	store, c := completeStoreSetup(t, markUpload(), putParts(t, be, 2), nil)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)
	be.PutErr = errors.New("be write failed")

	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1, 2)); err == nil {
		t.Fatal("expected CompleteMultipartUpload to fail")
	}
	want := []string{markBegin, markEnd}
	if got := c.events(); !slices.Equal(got, want) {
		t.Errorf("mark events = %v, want %v", got, want)
	}
}

// TestCompleteMultipartUpload_BeginErrorSurfaces asserts a store failure
// taking the mark fails the completion before anything is assembled.
func TestCompleteMultipartUpload_BeginErrorSurfaces(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	parts := putParts(t, be, 1)
	sentinel := errors.New("db error")
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).Return(markUpload(), nil).AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), gomock.Any()).Return(parts, nil).AnyTimes()
	store.EXPECT().BeginMultipartCompletion(gomock.Any(), "upload-1", gomock.Any()).Return(false, sentinel)
	c := multipartStubs(t, store)
	storetest.Permissive(store)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1)); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the Begin failure", err)
	}
	if len(c.recordObject) != 0 || len(c.events()) != 0 {
		t.Errorf("records=%d events=%v, want nothing after a failed Begin", len(c.recordObject), c.events())
	}
}

// TestCompleteMultipartUpload_BeginPassesStaleCutoff asserts the mark is
// taken over only from a completion that stopped renewing a grace period ago.
func TestCompleteMultipartUpload_BeginPassesStaleCutoff(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	parts := putParts(t, be, 1)
	var cutoff time.Time
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).Return(markUpload(), nil).AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), gomock.Any()).Return(parts, nil).AnyTimes()
	store.EXPECT().BeginMultipartCompletion(gomock.Any(), "upload-1", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, staleBefore time.Time) (bool, error) {
			cutoff = staleBefore
			return true, nil
		})
	multipartStubs(t, store)
	storetest.Permissive(store)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	before := time.Now()
	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1)); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	assertStaleCutoff(t, cutoff, before)
}

// TestCompleteMultipartUpload_EndFailureKeepsObject asserts a mark that could
// not be cleared does not fail a completion whose object is committed.
func TestCompleteMultipartUpload_EndFailureKeepsObject(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	parts := putParts(t, be, 1)
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).Return(markUpload(), nil).AnyTimes()
	store.EXPECT().GetParts(gomock.Any(), gomock.Any()).Return(parts, nil).AnyTimes()
	store.EXPECT().EndMultipartCompletion(gomock.Any(), "upload-1").Return(errors.New("db error"))
	c := multipartStubs(t, store)
	storetest.Permissive(store)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	if _, err := mgr.CompleteMultipartUpload(context.Background(), "multi", "key", "upload-1", partsOf(1)); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if len(c.recordObject) != 1 || !be.HasCopyOf("multi/key") {
		t.Errorf("records=%d, want the assembled object committed", len(c.recordObject))
	}
}

// -------------------------------------------------------------------------
// RENEWAL
// -------------------------------------------------------------------------

// markManager returns a Manager holding only the store and logger that
// withCompletionMark uses.
func markManager(store Stores) *Manager {
	return &Manager{stores: store, log: slog.New(slog.DiscardHandler)}
}

// TestWithCompletionMark_RenewsWhileAssembling asserts the mark is renewed on
// every tick while the assembly runs and cleared once it returns.
func TestWithCompletionMark_RenewsWhileAssembling(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		store := storetest.NewMockMetadataStore(gomock.NewController(t))
		store.EXPECT().RenewMultipartCompletion(gomock.Any(), "upload-1").Return(true, nil).Times(2)
		store.EXPECT().EndMultipartCompletion(gomock.Any(), "upload-1").Return(nil)

		etag, err := markManager(store).withCompletionMark(context.Background(), "upload-1", func(context.Context) (string, error) {
			time.Sleep(2*completionRenewEvery + completionRenewEvery/2)
			return "etag", nil
		})
		if err != nil || etag != "etag" {
			t.Errorf("withCompletionMark = %q, %v; want the assembly's result", etag, err)
		}
	})
}

// TestWithCompletionMark_LostRenewalCancelsAssembly asserts a renewal that
// finds the mark gone or fails cancels the assembly, whose parts are no longer
// protected, and the mark is still cleared.
func TestWithCompletionMark_LostRenewalCancelsAssembly(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("db error")
	cases := map[string]struct {
		held      bool
		renewErr  error
		wantCause error
	}{
		"mark gone":     {false, nil, errCompletionMarkLost},
		"renewal error": {false, sentinel, sentinel},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				store := storetest.NewMockMetadataStore(gomock.NewController(t))
				store.EXPECT().RenewMultipartCompletion(gomock.Any(), "upload-1").Return(tc.held, tc.renewErr)
				store.EXPECT().EndMultipartCompletion(gomock.Any(), "upload-1").Return(nil)

				start := time.Now()
				_, err := markManager(store).withCompletionMark(context.Background(), "upload-1", func(ctx context.Context) (string, error) {
					<-ctx.Done()
					return "", context.Cause(ctx)
				})
				if !errors.Is(err, tc.wantCause) {
					t.Errorf("err = %v, want the assembly cancelled by %v", err, tc.wantCause)
				}
				if waited := time.Since(start); waited != completionRenewEvery {
					t.Errorf("cancelled after %v, want the first renewal at %v", waited, completionRenewEvery)
				}
			})
		})
	}
}

// -------------------------------------------------------------------------
// PART UPLOAD AND ABORT
// -------------------------------------------------------------------------

// TestUploadPart_RefusedWhileCompleting asserts a part upload passes the
// stale cutoff to the store and surfaces ErrCompletionInProgress when the
// store refuses it, discarding the bytes it wrote.
func TestUploadPart_RefusedWhileCompleting(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).Return(markUpload(), nil).AnyTimes()
	c := &multipartCalls{}
	store.EXPECT().RecordPart(gomock.Any(), gomock.Any()).
		DoAndReturn(stubRecordPart(c, nil, core.ErrCompletionInProgress))
	store.EXPECT().DiscardPending(gomock.Any(), gomock.Any(), reasonPartUnrecorded).
		DoAndReturn(stubDiscardPending(c))
	storetest.Permissive(store)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	before := time.Now()
	_, err := mgr.UploadPart(context.Background(), "multi", "key", "upload-1", 1, bytes.NewReader([]byte("part-data")), 9)
	if !errors.Is(err, core.ErrCompletionInProgress) {
		t.Fatalf("err = %v, want ErrCompletionInProgress", err)
	}
	if len(c.recordPart) != 1 {
		t.Fatalf("RecordPart calls = %d, want 1", len(c.recordPart))
	}
	assertStaleCutoff(t, c.recordPart[0].staleBefore, before)
	if len(c.discarded) != 1 {
		t.Errorf("discarded = %+v, want the refused part's bytes", c.discarded)
	}
}

// TestAbortMultipartUpload_RefusedWhileCompleting asserts an abort passes the
// stale cutoff to the store, surfaces ErrCompletionInProgress when the store
// refuses it, and leaves the parts the completion is reading.
func TestAbortMultipartUpload_RefusedWhileCompleting(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	parts := putParts(t, be, 1)
	var cutoff time.Time
	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetMultipartUpload(gomock.Any(), gomock.Any()).Return(markUpload(), nil).AnyTimes()
	store.EXPECT().DeleteMultipartUpload(gomock.Any(), "upload-1", reasonAbortPart, gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, staleBefore time.Time) ([]core.CleanupItem, error) {
			cutoff = staleBefore
			return nil, core.ErrCompletionInProgress
		})
	storetest.Permissive(store)
	mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

	before := time.Now()
	if err := mgr.AbortMultipartUpload(context.Background(), "multi", "key", "upload-1"); !errors.Is(err, core.ErrCompletionInProgress) {
		t.Fatalf("err = %v, want ErrCompletionInProgress", err)
	}
	assertStaleCutoff(t, cutoff, before)
	if !be.Has(parts[0].Path("upload-1")) {
		t.Error("part bytes must stay while a completion holds the mark")
	}
}
