// -------------------------------------------------------------------------------
// Integration Tests - Write Fan-Out
//
// Author: Alex Freidah
//
// Pins the invariant a write placing its own copies has to hold: every copy the
// ledger claims is really on its backend, holding this write's bytes. Reading
// through the orchestrator cannot show that, because a read failing over to a
// good copy hides a broken one - so these tests read each copy off its backend
// directly, which is what the scrubber would otherwise be left to discover.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/testutil/testx"
)

// fanoutSpec is a three-backend fleet placing two copies per write, which is
// the smallest fleet where the copy a write places and the copy it displaces
// can land on different backends.
func fanoutSpec() harnessSpec {
	return harnessSpec{
		Backends: []harnessBackend{
			{Name: "h-minio-1", Quota: 64 << 20},
			{Name: "h-minio-2", Quota: 64 << 20},
			{Name: "h-minio-3", Quota: 64 << 20},
		},
		CopiesPerWrite: 2,
	}
}

// assertEveryCopyReadable checks each backend the ledger names for key really
// holds want, read off the backend rather than through the orchestrator.
//
// The copies after the first commit once their uploads finish, which is after
// the client has been answered, so the row count is waited on rather than read
// once. A copy that never commits fails the wait rather than passing quietly.
func assertEveryCopyReadable(t *testing.T, h *harness, key string, want []byte) {
	t.Helper()
	var backends []string
	testx.Eventually(t, 10*time.Second, func() bool {
		backends = h.objectBackends(key)
		return len(backends) == 2
	}, "ledger settled at %d copies of %s, want 2", len(h.objectBackends(key)), key)
	for _, name := range backends {
		be, ok := h.backends[name]
		if !ok {
			t.Fatalf("ledger names backend %q, which is not in the fleet", name)
		}
		res, err := be.GetObject(context.Background(), internalKey(key), "")
		if err != nil {
			t.Errorf("copy on %s is recorded but unreadable: %v", name, err)
			continue
		}
		got, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Errorf("copy on %s: read body: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("copy on %s holds %d bytes, want the %d this write sent", name, len(got), len(want))
		}
	}
}

// TestInt_ParallelCopies_EveryRecordedCopyExists covers the plain case: a write
// places two copies and both are on their backends.
func TestInt_ParallelCopies_EveryRecordedCopyExists(t *testing.T) {
	h := newHarness(t, fanoutSpec())
	key := uniqueKey(t, "fanout-fresh")
	body := bytes.Repeat([]byte("fan-out"), 512)

	h.put(key, body)
	assertEveryCopyReadable(t, h, key, body)
}

// TestInt_ParallelCopies_OverwriteKeepsBothCopies is the regression this suite
// exists for. An overwrite displaces the previous version's copies, and the
// backend it displaces from is one this write is still uploading its own copy
// to. Deleting the displaced bytes there takes the new copy with them and
// leaves a row describing an object that is gone - which no read through the
// orchestrator reveals, because it fails over to the copy that survived.
func TestInt_ParallelCopies_OverwriteKeepsBothCopies(t *testing.T) {
	h := newHarness(t, fanoutSpec())
	key := uniqueKey(t, "fanout-overwrite")

	first := bytes.Repeat([]byte("first-version"), 256)
	h.put(key, first)
	assertEveryCopyReadable(t, h, key, first)

	second := bytes.Repeat([]byte("second-version"), 512)
	h.put(key, second)
	assertEveryCopyReadable(t, h, key, second)

	// The client's own read must agree with what the copies hold, so a
	// surviving copy of the previous version cannot pass as the current one.
	if got := h.get(key); !bytes.Equal(got, second) {
		t.Errorf("GetObject returned %d bytes, want the %d the overwrite wrote", len(got), len(second))
	}
}

// TestInt_ParallelCopies_RepeatedOverwritesLeaveNoPhantom runs the overwrite
// several times over, since the copy a write places and the copy it displaces
// only collide on some placements and one pass can miss it.
func TestInt_ParallelCopies_RepeatedOverwritesLeaveNoPhantom(t *testing.T) {
	h := newHarness(t, fanoutSpec())
	key := uniqueKey(t, "fanout-rewrite")

	for i := range 6 {
		body := bytes.Repeat([]byte{byte('a' + i)}, 1024*(i+1))
		h.put(key, body)
		assertEveryCopyReadable(t, h, key, body)
	}
}
