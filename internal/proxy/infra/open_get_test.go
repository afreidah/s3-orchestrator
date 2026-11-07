// -------------------------------------------------------------------------------
// Backend Runtime - Opened GET Tests
//
// Author: Alex Freidah
//
// A GET opened ahead of its turn is timed only while it waits for its response,
// and its body gets the full backend timeout from when the caller starts
// reading it. Each test runs in a synctest bubble, so the timeouts cost fake time.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/counter"
)

// openTimeout is the backend timeout the opened GETs run under.
const openTimeout = 100 * time.Millisecond

// ctxBackend answers each GET with a body that reads as empty until the GET's
// context ends, and then fails with that context's error. When holdOpen is
// set, a GET instead waits for its context to end and fails.
type ctxBackend struct {
	fakeBackend
	holdOpen bool
}

func (b ctxBackend) GetObject(ctx context.Context, _, _ string) (*backend.GetObjectResult, error) {
	if b.holdOpen {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &backend.GetObjectResult{Body: ctxBody{ctx}}, nil
}

// ctxBody is the body ctxBackend answers with.
type ctxBody struct{ ctx context.Context }

func (b ctxBody) Read([]byte) (int, error) { return 0, b.ctx.Err() }
func (ctxBody) Close() error               { return nil }

// openGetRuntime is a runtime that charges a local counter under openTimeout.
func openGetRuntime() *BackendRuntime {
	usage := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1"}), nil)
	return New(&Config{BackendTimeout: openTimeout, Usage: usage})
}

// TestOpenGet_TimesTheBodyFromStart verifies a body waiting for its turn is not
// timed, and gets the full timeout from when the caller starts reading it.
func TestOpenGet_TimesTheBodyFromStart(t *testing.T) {
	rt := openGetRuntime()
	synctest.Test(t, func(t *testing.T) {
		result, start, release, err := rt.OpenGet(t.Context(), "b1", ctxBackend{}, "k")
		if err != nil {
			t.Fatalf("OpenGet: %v", err)
		}
		defer release()
		time.Sleep(3 * openTimeout)
		if _, err := result.Body.Read(nil); err != nil {
			t.Fatalf("body ended while waiting for its turn: %v", err)
		}
		start()
		time.Sleep(openTimeout - time.Millisecond)
		if _, err := result.Body.Read(nil); err != nil {
			t.Fatalf("body ended before its timeout: %v", err)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if _, err := result.Body.Read(nil); !errors.Is(err, context.Canceled) {
			t.Errorf("read after the timeout = %v, want the GET cancelled", err)
		}
	})
}

// TestOpenGet_TimesTheWaitForTheResponse verifies a backend that never answers
// fails the GET at the timeout.
func TestOpenGet_TimesTheWaitForTheResponse(t *testing.T) {
	rt := openGetRuntime()
	synctest.Test(t, func(t *testing.T) {
		began := time.Now()
		if _, _, _, err := rt.OpenGet(t.Context(), "b1", ctxBackend{holdOpen: true}, "k"); err == nil {
			t.Fatal("OpenGet succeeded against a backend that never answered")
		}
		if took := time.Since(began); took != openTimeout {
			t.Errorf("gave up after %s, want %s", took, openTimeout)
		}
	})
}
