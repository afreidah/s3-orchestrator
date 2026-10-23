// -------------------------------------------------------------------------------
// Object Manager Tests - Egress on the Bytes the Backend Sent
//
// Author: Alex Freidah
//
// A backend that ignores Range sends more than the range it was asked for, and
// the backend layer cuts the body down while recording the transfer in
// WireSize. These tests pin that both read paths judge and charge egress on
// that transfer rather than on the body handed to the client.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/proxy/readpath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// wirePad is how many bytes beyond each body paddedWire reports as sent.
const wirePad = 1000

// paddedWire reports every GET as having transferred wirePad bytes more than
// its body, the way a range cut from a whole-object reply does.
type paddedWire struct {
	backend.ObjectBackend
}

// GetObject forwards to the wrapped backend and pads the reported transfer.
func (p paddedWire) GetObject(ctx context.Context, key, rangeHeader string) (*backend.GetObjectResult, error) {
	r, err := p.ObjectBackend.GetObject(ctx, key, rangeHeader)
	if err == nil {
		r.WireSize = r.Size + wirePad
	}
	return r, err
}

// TestMeteredGet_JudgesLimitOnWireBytes declines a response whose body fits
// the egress limit but whose transfer does not, and charges the refused call.
func TestMeteredGet_JudgesLimitOnWireBytes(t *testing.T) {
	t.Parallel()
	be := backendtest.NewInMemory()
	if _, err := be.PutObject(context.Background(), "k", bytes.NewReader([]byte("0123456789")), 10, "", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rt := newFakeRangeRuntime("be1", requestCapped(t, 0, wirePad/2, 0))

	_, _, err := meteredGet(context.Background(), rt, paddedWire{be}, "be1", "k", "bytes=0-1")
	if !errors.Is(err, readpath.ErrUsageLimitSkip) {
		t.Fatalf("err = %v, want ErrUsageLimitSkip", err)
	}
	var u core.Usage
	rt.usage.Backend().Load("be1", time.Time{}, &u)
	if u.APIRequests != 1 || u.EgressBytes != 0 {
		t.Errorf("usage = %d calls, %d egress; want 1 call and no egress", u.APIRequests, u.EgressBytes)
	}
}

// TestStoredRangeFetcher_ChargesWireBytes charges a frame fetch the bytes the
// backend sent, not the frame it returned.
func TestStoredRangeFetcher_ChargesWireBytes(t *testing.T) {
	t.Parallel()
	c := newTestCodec(t)
	s := storeCompressed(t, c, compressibleFixture(fetchTestChunk*2), false)
	f := newStoredRangeFetcher(s.rt, paddedWire{s.be}, s.enc, s.loc, fetchTestKey, "be1")

	if _, err := f.FetchRange(context.Background(), 0, 15); err != nil {
		t.Fatalf("FetchRange: %v", err)
	}
	var u core.Usage
	s.rt.usage.Backend().Load("be1", time.Time{}, &u)
	if u.EgressBytes != 16+wirePad {
		t.Errorf("egress = %d, want %d", u.EgressBytes, 16+wirePad)
	}
}

// TestGet_ChargesWireBytes charges a ranged GET of a plain copy the bytes the
// backend sent, while the client still receives only its range.
func TestGet_ChargesWireBytes(t *testing.T) {
	t.Parallel()
	const key, beName = "bucket/wire.txt", "b1"
	full := []byte("0123456789abcdefghij")

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetAllObjectLocations(gomock.Any(), key).Return([]core.ObjectLocation{{
		ObjectKey: key, BackendName: beName, SizeBytes: int64(len(full)),
	}}, nil).AnyTimes()
	storetest.Permissive(store)

	be := backendtest.NewInMemory()
	if _, err := be.PutObject(context.Background(), key, bytes.NewReader(full), int64(len(full)), "text/plain", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: paddedWire{be}}, nil)

	res, err := f.GetObject(context.Background(), key, "bytes=5-9")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	got, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(got) != "56789" {
		t.Errorf("body = %q, want 56789", got)
	}
	if egress := usageOf(f, beName).EgressBytes; egress != 5+wirePad {
		t.Errorf("egress = %d, want %d", egress, 5+wirePad)
	}
}
