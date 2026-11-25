// -------------------------------------------------------------------------------
// Write Coordinator - Rewrite Tests
//
// Author: Alex Freidah
//
// Drives RewriteCopies over in-memory backends and a real codec, with the
// store mocked, so the tests can see which copies were read, what each
// backend was sent, and what every claim and placement carried.
// -------------------------------------------------------------------------------

package writepath

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/storedform"
)

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// countingBackend counts the GETs an in-memory backend serves, so a test can
// tell which copies a rewrite read.
type countingBackend struct {
	*backendtest.InMemory
	gets int
}

// GetObject counts the read and serves it.
func (c *countingBackend) GetObject(ctx context.Context, key, rangeHeader string) (*s3be.GetObjectResult, error) {
	c.gets++
	return c.InMemory.GetObject(ctx, key, rangeHeader)
}

// rewriteFleet is a coordinator over two backends, b1 and b2, each holding a
// verbatim copy of payload, and the copies' rows in listing order.
type rewriteFleet struct {
	coord  *Coordinator
	b1, b2 *countingBackend
	copies []core.ObjectLocation
	layers *storedform.Layers
}

// newRewriteFleet seeds payload on both backends and builds a coordinator
// whose layers compress, with store behind it.
func newRewriteFleet(t *testing.T, store CoordinatorStores, payload []byte) *rewriteFleet {
	t.Helper()
	codec, err := compression.NewCodecForLevel(config.DefaultCompressionLevel, compression.MinChunkSize)
	if err != nil {
		t.Fatalf("NewCodecForLevel: %v", err)
	}
	t.Cleanup(codec.Close)
	f := &rewriteFleet{
		b1:     &countingBackend{InMemory: backendtest.NewInMemory()},
		b2:     &countingBackend{InMemory: backendtest.NewInMemory()},
		layers: &storedform.Layers{Codec: codec, Compression: config.CompressionConfig{MinRatio: 0.9, Level: config.DefaultCompressionLevel}},
	}
	sum := sha256.Sum256(payload)
	for _, name := range []string{"b1", "b2"} {
		be := map[string]*countingBackend{"b1": f.b1, "b2": f.b2}[name]
		loc := core.ObjectLocation{
			ObjectKey: "k", BackendName: name, StorageKey: "k!" + name, SizeBytes: int64(len(payload)),
			StoredForm: core.StoredForm{ContentHash: hex.EncodeToString(sum[:])},
		}
		if _, err := be.PutObject(context.Background(), loc.StorageKey, bytes.NewReader(payload), loc.SizeBytes, "text/plain", nil); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		f.copies = append(f.copies, loc)
	}
	names := []string{"b1", "b2"}
	rt := infra.New(&infra.Config{
		Backends:       map[string]s3be.ObjectBackend{"b1": f.b1, "b2": f.b2},
		Order:          names,
		BackendTimeout: 5 * time.Second,
		Usage:          counter.NewUsageTracker(counter.NewLocalCounterBackend(names), nil),
		Quota:          counter.NewQuotaTracker(),
	})
	f.coord = New(rt, store)
	return f
}

// rewrite rewrites the fleet's copies compressed.
func (f *rewriteFleet) rewrite() []error {
	return f.coord.RewriteCopies(context.Background(), &RewriteRequest{
		Copies: f.copies,
		Layers: f.layers,
		Want:   core.StoredCompressed,
	})
}

// placed is what one placement recorded: the copy it replaced and the intent
// that took its place.
type placed struct {
	source core.ObjectLocation
	intent core.PendingObject
}

// expectRewritePlacements claims and places every rewritten copy, recording
// each placement in order.
func expectRewritePlacements(store *MockCoordinatorStores, n int) *[]placed {
	got := new([]placed)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), gomock.Any(), core.AcceptForRewrite).Return(true, nil).Times(n)
	store.EXPECT().PlaceCopy(gomock.Any(), gomock.Any()).Times(n).
		DoAndReturn(func(_ context.Context, p *core.Placement) (int64, []core.CleanupItem, error) {
			if !p.RemoveSource || !p.Reformed || p.Reasons != rewriteMove.Reasons {
				return 0, nil, errors.New("placement is not a rewrite")
			}
			*got = append(*got, placed{source: *p.Source, intent: *p.Intent})
			return p.Intent.SizeBytes, nil, nil
		})
	return got
}

// compressible is a payload zstd shrinks well past the fleet's ratio.
var compressible = bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 200)

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestRewriteCopies_RewritesEveryCopyFromOneRead verifies an object's copies
// are rewritten from a single read: each lands on its own backend at a new
// path, claimed and placed at the encoded size, and decodes to the payload.
func TestRewriteCopies_RewritesEveryCopyFromOneRead(t *testing.T) {
	t.Parallel()
	store := NewMockCoordinatorStores(gomock.NewController(t))
	got := expectRewritePlacements(store, 2)
	f := newRewriteFleet(t, store, compressible)

	for i, err := range f.rewrite() {
		if err != nil {
			t.Fatalf("copy %d: %v", i, err)
		}
	}
	if f.b1.gets != 1 || f.b2.gets != 0 {
		t.Errorf("reads = b1 %d, b2 %d, want one read of the first copy", f.b1.gets, f.b2.gets)
	}
	for i := range *got {
		f.assertRewritten(t, &f.copies[i], &(*got)[i])
	}
}

// assertRewritten checks p replaced want with an encoded copy on want's own
// backend, recorded at the encoded size and decoding to the payload.
func (f *rewriteFleet) assertRewritten(t *testing.T, want *core.ObjectLocation, p *placed) {
	t.Helper()
	name := p.intent.BackendName
	if p.source.BackendName != want.BackendName || name != want.BackendName {
		t.Errorf("replaced %s with a copy on %s, want %s onto itself", p.source.BackendName, name, want.BackendName)
	}
	if !p.intent.Form().Has(core.StoredCompressed) || p.intent.SizeBytes >= int64(len(compressible)) {
		t.Errorf("%s recorded form %v at %d bytes, want the encoding", name, p.intent.Form(), p.intent.SizeBytes)
	}
	be := map[string]*countingBackend{"b1": f.b1, "b2": f.b2}[name]
	res, err := be.GetObject(context.Background(), p.intent.StorageKey, "")
	if err != nil {
		t.Fatalf("read back %s: %v", name, err)
	}
	plain, closeFn, err := storedform.Open(context.Background(), nil, f.layers.Codec, res.Body, &p.intent.StoredForm)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer closeFn()
	if data, _ := io.ReadAll(plain); !bytes.Equal(data, compressible) {
		t.Errorf("%s holds bytes that do not decode to the payload", name)
	}
}

// TestRewriteCopies_FailsOverToTheNextSource verifies a copy that cannot be
// read gives way to the next one, and is still rewritten from it.
func TestRewriteCopies_FailsOverToTheNextSource(t *testing.T) {
	t.Parallel()
	store := NewMockCoordinatorStores(gomock.NewController(t))
	got := expectRewritePlacements(store, 2)
	f := newRewriteFleet(t, store, compressible)
	f.b1.GetErr = errors.New("b1 unreachable")

	for i, err := range f.rewrite() {
		if err != nil {
			t.Fatalf("copy %d: %v", i, err)
		}
	}
	if f.b2.gets != 1 || len(*got) != 2 {
		t.Errorf("b2 read %d times and %d copies placed, want b2 read once and both placed", f.b2.gets, len(*got))
	}
}

// TestRewriteCopies_MismatchedHashWritesNothing verifies bytes that do not
// hash to the content hash are reported and never written, trying each copy
// before every one fails with the mismatch.
func TestRewriteCopies_MismatchedHashWritesNothing(t *testing.T) {
	t.Parallel()
	store := NewMockCoordinatorStores(gomock.NewController(t))
	f := newRewriteFleet(t, store, compressible)
	for i := range f.copies {
		f.copies[i].ContentHash = "0000"
	}
	before := promtest.ToFloat64(telemetry.IntegrityErrorsTotal.WithLabelValues(integrityOpRewrite))

	for i, err := range f.rewrite() {
		if !errors.Is(err, storedform.ErrContentMismatch) {
			t.Errorf("copy %d: err = %v, want the content mismatch", i, err)
		}
	}
	if f.b1.gets != 1 || f.b2.gets != 1 {
		t.Errorf("reads = b1 %d, b2 %d, want each copy tried once", f.b1.gets, f.b2.gets)
	}
	if f.b1.PutCalls+f.b2.PutCalls != 2 {
		t.Errorf("backends took %d writes beyond the seeds, want none", f.b1.PutCalls+f.b2.PutCalls-2)
	}
	if got := promtest.ToFloat64(telemetry.IntegrityErrorsTotal.WithLabelValues(integrityOpRewrite)) - before; got < 2 {
		t.Errorf("rewrite integrity errors rose by %v, want at least 2", got)
	}
}

// TestRewriteCopies_DeclineEndsEveryCopy verifies an encoding that misses the
// ratio is every copy's answer from one read, since no other copy would
// encode better.
func TestRewriteCopies_DeclineEndsEveryCopy(t *testing.T) {
	t.Parallel()
	store := NewMockCoordinatorStores(gomock.NewController(t))
	f := newRewriteFleet(t, store, compressible)
	f.layers.Compression.MinRatio = 0.0001

	for i, err := range f.rewrite() {
		if _, ok := errors.AsType[*storedform.Declined](err); !ok {
			t.Errorf("copy %d: err = %v, want the decline", i, err)
		}
	}
	if f.b1.gets != 1 || f.b2.gets != 0 {
		t.Errorf("reads = b1 %d, b2 %d, want the decline from one read", f.b1.gets, f.b2.gets)
	}
}

// TestRewriteCopies_EachCopyLandsOnItsOwn verifies a copy whose backend has no
// room, and one whose write fails, leave the others rewritten: the refused
// copy writes nothing and the failed write discards its intent.
func TestRewriteCopies_EachCopyLandsOnItsOwn(t *testing.T) {
	t.Parallel()
	store := NewMockCoordinatorStores(gomock.NewController(t))
	onB1 := gomock.Cond(func(p *core.PendingObject) bool { return p.BackendName == "b1" })
	onB2 := gomock.Cond(func(p *core.PendingObject) bool { return p.BackendName == "b2" })
	store.EXPECT().InsertPendingIfFits(gomock.Any(), onB1, core.AcceptForRewrite).Return(false, nil)
	store.EXPECT().InsertPendingIfFits(gomock.Any(), onB2, core.AcceptForRewrite).Return(true, nil)
	store.EXPECT().DiscardPending(gomock.Any(), onB2, rewriteMove.Reasons.Orphan).Return(nil, nil)
	f := newRewriteFleet(t, store, compressible)
	f.b2.PutErr = errors.New("b2 write failed")

	errs := f.rewrite()
	if !errors.Is(errs[0], core.ErrTargetRefused) {
		t.Errorf("b1: err = %v, want the refused claim", errs[0])
	}
	if !s3be.IsCopyPhase(errs[1], s3be.CopyPhaseWrite) {
		t.Errorf("b2: err = %v, want the write failure", errs[1])
	}
	if f.b1.PutCalls != 1 {
		t.Errorf("b1 took %d writes beyond its seed, want none", f.b1.PutCalls-1)
	}
}

// TestRewriteCopies_UnhashedSourceIsNotChecked verifies a copy with no content
// hash is rewritten without a check, since there is nothing to check against:
// judged against an empty hash it would fail.
func TestRewriteCopies_UnhashedSourceIsNotChecked(t *testing.T) {
	t.Parallel()
	store := NewMockCoordinatorStores(gomock.NewController(t))
	expectRewritePlacements(store, 1)
	f := newRewriteFleet(t, store, compressible)
	f.copies = f.copies[:1]
	f.copies[0].ContentHash = ""

	if errs := f.rewrite(); errs[0] != nil {
		t.Fatalf("rewrite: %v", errs[0])
	}
}
