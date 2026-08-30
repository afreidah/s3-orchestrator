// -------------------------------------------------------------------------------
// Compressed Read Tests
//
// Author: Alex Freidah
//
// Two properties, and they pull against each other. Clients must see the object
// exactly as they wrote it, whatever the stored form; and a range must cost the
// frames it covers rather than the object. The second is asserted on bytes
// fetched from the backend, not on elapsed time, because time measures the host
// and bytes measure the thing that is billed.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// seedCreatedAt is the row's creation time. The in-memory backend reports no
// modification time, which is also true of some real ones, so this is what the
// response has to fall back to.
var seedCreatedAt = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// countingBackend totals the bytes it hands back, which is the unit the
// proportionality assertion is stated in.
type countingBackend struct {
	*backendtest.InMemory
	served int64
	calls  int
}

// GetObject forwards and tallies what left the backend.
func (c *countingBackend) GetObject(ctx context.Context, key, rangeHeader string) (*backend.GetObjectResult, error) {
	r, err := c.InMemory.GetObject(ctx, key, rangeHeader)
	if err == nil {
		c.served += r.Size
		c.calls++
	}
	return r, err
}

// compressedFixture is one compressed object on a backend with the row that
// describes it.
type compressedFixture struct {
	be    *countingBackend
	fleet *fleet
	src   []byte
	loc   core.ObjectLocation
}

// seedCompressed writes src compressed (and optionally encrypted) straight onto
// a backend, then builds a fleet whose ledger points at it. The write path is
// covered elsewhere; this states the stored form directly so a read test is not
// also a write test.
func seedCompressed(t *testing.T, src []byte, enc *encryption.Encryptor) *compressedFixture {
	t.Helper()
	const key, beName = "bucket/obj.bin", "b1"
	ctx := context.Background()

	codec := newPutCodec(t)
	var compressed bytes.Buffer
	if _, err := codec.Compress(&compressed, bytes.NewReader(src)); err != nil {
		t.Fatalf("Compress: %v", err)
	}

	loc := core.ObjectLocation{
		ObjectKey:                key,
		BackendName:              beName,
		CreatedAt:                seedCreatedAt,
		PlaintextSize:            int64(compressed.Len()),
		LogicalSize:              int64(len(src)),
		CompressionAlgorithm:     compression.Algorithm,
		CompressionFormatVersion: compression.FormatVersion,
	}
	stored := compressed.Bytes()
	if enc != nil {
		res, err := enc.Encrypt(ctx, bytes.NewReader(stored), int64(len(stored)))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if stored, err = io.ReadAll(res.Body); err != nil {
			t.Fatalf("read ciphertext: %v", err)
		}
		loc.Encrypted = true
		loc.EncryptionKey = encryption.PackKeyData(res.BaseNonce, res.WrappedDEK)
		loc.KeyID = res.KeyID
	}
	loc.SizeBytes = int64(len(stored))

	be := &countingBackend{InMemory: backendtest.NewInMemory()}
	if _, err := be.PutObject(ctx, key, bytes.NewReader(stored), int64(len(stored)), "application/octet-stream", nil); err != nil {
		t.Fatalf("seed backend: %v", err)
	}

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetAllObjectLocations(gomock.Any(), key).Return([]core.ObjectLocation{loc}, nil).AnyTimes()
	storetest.Permissive(store)

	f := newFleet(t, store, map[string]backend.ObjectBackend{beName: be}, &fleetOpts{
		Order:     []string{beName},
		Codec:     codec,
		Encryptor: enc,
	})
	return &compressedFixture{be: be, fleet: f, src: src, loc: loc}
}

// get reads a range (or the whole object when rangeHeader is empty).
func (c *compressedFixture) get(t *testing.T, rangeHeader string) *backend.GetObjectResult {
	t.Helper()
	res, err := c.fleet.GetObject(context.Background(), c.loc.ObjectKey, rangeHeader)
	if err != nil {
		t.Fatalf("GetObject(%q): %v", rangeHeader, err)
	}
	return res.GetObjectResult
}

// TestCompressedGet_ServesTheObjectAsWritten is the headline of #1256: whatever
// the stored form, the client gets back exactly what it sent, at the size it
// sent.
func TestCompressedGet_ServesTheObjectAsWritten(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		enc  bool
	}{{name: "compressed"}, {name: "compressed and encrypted", enc: true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var enc *encryption.Encryptor
			if tc.enc {
				enc = newTestEncryptor(t)
			}
			fx := seedCompressed(t, compressibleBody(putCompressChunk*3+17), enc)

			res := fx.get(t, "")
			if res.Size != int64(len(fx.src)) {
				t.Errorf("Size = %d, want the logical %d", res.Size, len(fx.src))
			}
			got, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			_ = res.Body.Close()
			if !bytes.Equal(got, fx.src) {
				t.Error("the object did not read back as written")
			}
		})
	}
}

// TestCompressedHead_ReportsLogicalSize covers the other half of #1256. A HEAD
// reporting the stored size sends clients ranging against coordinates the
// object does not have.
func TestCompressedHead_ReportsLogicalSize(t *testing.T) {
	t.Parallel()
	fx := seedCompressed(t, compressibleBody(putCompressChunk*2), nil)

	res, err := fx.fleet.HeadObject(context.Background(), fx.loc.ObjectKey)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if res.Size != int64(len(fx.src)) {
		t.Errorf("Size = %d, want the logical %d (stored is %d)", res.Size, len(fx.src), fx.loc.SizeBytes)
	}
}

// TestCompressedGet_RangeFetchesOnlyTheFramesItCovers is the acceptance
// criterion for #1257, asserted on bytes off the backend rather than on time.
func TestCompressedGet_RangeFetchesOnlyTheFramesItCovers(t *testing.T) {
	t.Parallel()
	// Incompressible, so the stored object is large enough that fetching all of
	// it would be obvious against the budget below.
	src := make([]byte, putCompressChunk*16)
	rnd := rand.New(rand.NewSource(1256)) //nolint:gosec // G404: deterministic fixture, not security material
	if _, err := rnd.Read(src); err != nil {
		t.Fatalf("seed random: %v", err)
	}
	fx := seedCompressed(t, src, nil)

	const readLen = 1024
	offset := int64(putCompressChunk * 9)
	res := fx.get(t, fmt.Sprintf("bytes=%d-%d", offset, offset+readLen-1))
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = res.Body.Close()

	if !bytes.Equal(got, src[offset:offset+readLen]) {
		t.Fatal("the range did not return the corresponding source bytes")
	}
	if res.ContentRange == "" {
		t.Error("a satisfied range carried no Content-Range")
	}

	// The seek table plus the frames a 1 KiB range can straddle. The stored
	// object is far larger, so fetching it whole blows this by an order of
	// magnitude.
	budget := int64(putCompressChunk * 4)
	t.Logf("served %d bytes of a %d byte stored object in %d fetches to satisfy a %d byte range",
		fx.be.served, fx.loc.SizeBytes, fx.be.calls, readLen)
	if fx.be.served > budget {
		t.Errorf("fetched %d bytes to serve %d bytes of a %d byte object, budget %d",
			fx.be.served, readLen, fx.loc.SizeBytes, budget)
	}
}

// TestCompressedGet_RandomRangesMatchTheSource walks ranges across frame
// boundaries, including the compressed-plus-encrypted combination the epic
// calls the highest-risk path: testing each pair separately never reaches it.
func TestCompressedGet_RandomRangesMatchTheSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		enc  bool
	}{{name: "compressed"}, {name: "compressed and encrypted", enc: true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var enc *encryption.Encryptor
			if tc.enc {
				enc = newTestEncryptor(t)
			}
			fx := seedCompressed(t, compressibleBody(putCompressChunk*5+613), enc)

			rnd := rand.New(rand.NewSource(1257)) //nolint:gosec // G404: deterministic fixture, not security material
			for range 25 {
				start := rnd.Int63n(int64(len(fx.src)))
				end := start + rnd.Int63n(int64(len(fx.src))-start)
				res := fx.get(t, fmt.Sprintf("bytes=%d-%d", start, end))
				got, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatalf("read bytes=%d-%d: %v", start, end, err)
				}
				_ = res.Body.Close()
				if !bytes.Equal(got, fx.src[start:end+1]) {
					t.Fatalf("bytes=%d-%d returned the wrong bytes", start, end)
				}
			}
		})
	}
}

// TestCompressedGet_UnsatisfiableRangeServesWholeObject matches what the
// uncompressed path does with a range it cannot translate: RFC 9110 allows
// ignoring a Range that cannot be acted on, and the whole object is the one
// safe answer.
func TestCompressedGet_UnsatisfiableRangeServesWholeObject(t *testing.T) {
	t.Parallel()
	fx := seedCompressed(t, compressibleBody(4096), nil)

	res := fx.get(t, "bytes=999999-1000000")
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = res.Body.Close()

	if !bytes.Equal(got, fx.src) {
		t.Error("an unsatisfiable range did not fall back to the whole object")
	}
	if res.ContentRange != "" {
		t.Errorf("ContentRange = %q, want empty for a whole-object response", res.ContentRange)
	}
}

// TestCompressedGet_CarriesBackendMetadata checks the metadata a compressed read
// has no whole-object GET to learn from. It is captured off the first frame
// fetch, so losing it would leave every compressed response without a content
// type or ETag.
func TestCompressedGet_CarriesBackendMetadata(t *testing.T) {
	t.Parallel()
	fx := seedCompressed(t, compressibleBody(4096), nil)

	res := fx.get(t, "")
	defer func() { _ = res.Body.Close() }()

	if res.ContentType != "application/octet-stream" {
		t.Errorf("ContentType = %q, want the stored one", res.ContentType)
	}
	if res.ETag == "" {
		t.Error("ETag is empty; conditional requests and If-Range depend on it")
	}
	// This backend reports no modification time, so the response has to fall
	// back to the row's creation time. A zero here is dropped by the transport,
	// and clients that require Last-Modified reject the listing outright.
	if !res.LastModified.Equal(seedCreatedAt) {
		t.Errorf("LastModified = %v, want the row's creation time %v", res.LastModified, seedCreatedAt)
	}
}

// TestLogicalSize_ByStoredForm pins which size a client is told about for each
// stored form. Getting this wrong sends clients ranging against coordinates the
// object does not have.
func TestLogicalSize_ByStoredForm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		loc  *core.ObjectLocation
		want int64
	}{
		{name: "nil row", loc: nil, want: 0},
		{name: "verbatim", loc: &core.ObjectLocation{SizeBytes: 100}, want: 100},
		{
			name: "encrypted only",
			loc:  &core.ObjectLocation{SizeBytes: 160, Encrypted: true, PlaintextSize: 100},
			want: 100,
		},
		{
			name: "compressed only",
			loc: &core.ObjectLocation{
				SizeBytes: 40, LogicalSize: 100, CompressionAlgorithm: compression.Algorithm,
			},
			want: 100,
		},
		{
			name: "compressed and encrypted",
			loc: &core.ObjectLocation{
				SizeBytes: 70, Encrypted: true, PlaintextSize: 40,
				LogicalSize: 100, CompressionAlgorithm: compression.Algorithm,
			},
			want: 100,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := logicalSize(tc.loc); got != tc.want {
				t.Errorf("logicalSize = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCompressedGet_WithoutCodecFailsOver covers a compressed row on a server
// with no codec wired. Serving the stored bytes would hand the client a zstd
// stream it never uploaded, so the read has to fail instead.
func TestCompressedGet_WithoutCodecFailsOver(t *testing.T) {
	t.Parallel()
	fx := seedCompressed(t, compressibleBody(4096), nil)
	fx.fleet.codec = nil

	if _, err := fx.fleet.GetObject(context.Background(), fx.loc.ObjectKey, ""); err == nil {
		t.Fatal("GetObject succeeded with no codec for a compressed object")
	}
}

// TestCompressedGet_DamagedFrameFailsTheRead checks that damaged stored bytes
// end the read rather than reaching the client as plausible garbage. The
// damage keeps the object's length, so the seek table still parses and the
// failure lands where it belongs: on decoding a frame.
func TestCompressedGet_DamagedFrameFailsTheRead(t *testing.T) {
	t.Parallel()
	fx := seedCompressed(t, compressibleBody(putCompressChunk*2), nil)

	obj := fx.be.Objects[fx.loc.ObjectKey]
	for i := 64; i < 512 && i < len(obj.Data); i++ {
		obj.Data[i] ^= 0xFF
	}
	fx.be.Objects[fx.loc.ObjectKey] = obj

	res, err := fx.fleet.GetObject(context.Background(), fx.loc.ObjectKey, "")
	if err == nil {
		// The table is intact, so the reader builds; the frame fails on read.
		_, err = io.ReadAll(res.Body)
		_ = res.Body.Close()
	}
	if err == nil {
		t.Fatal("a damaged frame was served without an error")
	}
	if !errors.Is(err, compression.ErrCorruptObject) {
		t.Errorf("err = %v, want it to name ErrCorruptObject", err)
	}
}

// TestCompressedGet_TruncatedObjectFailsTheRead covers a copy shorter than its
// row claims. It is deliberately not asserted as corruption: a short answer to
// a range inside the declared object is indistinguishable from a backend that
// cut the response, and reporting a transport fault as a bad copy is how
// healthy data gets condemned. Failing the read is the part that matters.
func TestCompressedGet_TruncatedObjectFailsTheRead(t *testing.T) {
	t.Parallel()
	fx := seedCompressed(t, compressibleBody(putCompressChunk*2), nil)

	obj := fx.be.Objects[fx.loc.ObjectKey]
	obj.Data = obj.Data[:len(obj.Data)/2]
	fx.be.Objects[fx.loc.ObjectKey] = obj

	if _, err := fx.fleet.GetObject(context.Background(), fx.loc.ObjectKey, ""); err == nil {
		t.Fatal("GetObject succeeded over a truncated compressed object")
	}
}

// TestCompressedGet_RejectsContradictoryRow covers a row whose encryption
// metadata contradicts itself. The read fails over to a sibling copy rather
// than doing range math on sizes that cannot both be right.
func TestCompressedGet_RejectsContradictoryRow(t *testing.T) {
	t.Parallel()
	fx := seedCompressed(t, compressibleBody(4096), nil)

	// A cleared flag beside a surviving key is the signature of a row that lost
	// its encryption metadata.
	bad := fx.loc
	bad.Encrypted = false
	bad.EncryptionKey = []byte("leftover")

	store := storetest.NewMockMetadataStore(gomock.NewController(t))
	store.EXPECT().GetAllObjectLocations(gomock.Any(), bad.ObjectKey).
		Return([]core.ObjectLocation{bad}, nil).AnyTimes()
	storetest.Permissive(store)

	f := newFleet(t, store, map[string]backend.ObjectBackend{"b1": fx.be}, &fleetOpts{
		Order: []string{"b1"},
		Codec: newPutCodec(t),
	})
	if _, err := f.GetObject(context.Background(), bad.ObjectKey, ""); err == nil {
		t.Fatal("GetObject succeeded on a row that contradicts itself")
	}
}
