// -------------------------------------------------------------------------------
// Integration Tests - Compressed Objects End to End
//
// Author: Alex Freidah
//
// Compression is supposed to be invisible: a client writes bytes and reads the
// same bytes back, whatever form the backend holds them in. Everything else in
// the feature - the seek table, the ratio floor, the logical size carried
// alongside the stored one - exists to keep that true while making partial
// reads cheap.
//
// These tests hold the whole feature to that promise against real backends,
// across the size boundaries the chunked format actually branches on, with
// encryption layered on top, and through the operations that move stored bytes
// around: replication, rebalance, drain, and the scrubber that is supposed to
// notice when those bytes stop being what the ledger says they are.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// compressionOn is the feature configuration these tests run with: the smallest
// chunk the codec allows, so a fixture of a few hundred kilobytes still spans
// enough frames for a ranged read to have a choice, and no size floor, so the
// boundary cases are decided by the ratio rather than by being small.
func compressionOn() *config.CompressionConfig {
	return &config.CompressionConfig{
		Enabled:   true,
		Level:     "default",
		ChunkSize: compTestChunk,
		MinSize:   1,
		MinRatio:  config.DefaultCompressionMinRatio,
	}
}

// -------------------------------------------------------------------------
// SIZE BOUNDARIES
// -------------------------------------------------------------------------

// TestCompression_RoundTripAcrossSizeBoundaries walks the sizes the chunked
// format branches on. A whole number of frames, one byte either side of a frame
// boundary, and the degenerate sizes at the bottom are where an off-by-one in
// the seek table or the tail frame would show up, and nowhere else.
//
// Every case asserts the same thing - the client reads back what it wrote - and
// additionally pins whether the object was stored encoded, because an object
// the ratio floor declined is stored verbatim and would satisfy every
// round-trip assertion without the encoded path running at all.
func TestCompression_RoundTripAcrossSizeBoundaries(t *testing.T) {
	h := newHarness(t, harnessSpec{Compression: compressionOn()})

	cases := []struct {
		name        string
		size        int
		wantEncoded bool
	}{
		// Nothing to encode, and nothing a seek table could describe.
		{"empty", 0, false},
		// Below the point where an encoding can pay for its own framing.
		{"single byte", 1, false},
		{"sub-chunk", compTestChunk / 2, true},
		{"exactly one chunk", compTestChunk, true},
		{"one byte over a chunk", compTestChunk + 1, true},
		{"one byte under a chunk", compTestChunk - 1, true},
		{"exactly two chunks", compTestChunk * 2, true},
		{"multi-chunk with a partial tail", compTestChunk*3 + 7, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := uniqueKey(t, "comp-boundary")
			body := compressible(tc.size)

			target := h.put(key, body)
			if got := h.get(key); !bytes.Equal(got, body) {
				t.Fatalf("read back %d bytes, want the %d written", len(got), len(body))
			}
			assertStoredForm(t, h, key, target, body, tc.wantEncoded)
		})
	}
}

// assertStoredForm requires an object to have been stored in the form the case
// expects and, when that form is an encoding, that the ledger and the backend
// agree on its size and the copy still decodes to what was written.
func assertStoredForm(t *testing.T, h *harness, key, target string, body []byte, wantEncoded bool) {
	t.Helper()

	algorithm := h.compressionAlgorithm(key)
	encoded := algorithm != ""
	if encoded != wantEncoded {
		t.Fatalf("stored with algorithm %q (encoded=%t), want encoded=%t",
			algorithm, encoded, wantEncoded)
	}
	if !encoded {
		return
	}

	// The ledger has to keep both sizes: the stored one is what the backend
	// holds and what quota is charged, the logical one is the only surviving
	// record of what the client wrote.
	stored := h.storedSize(key)
	if physical := h.backendSize(target, key); stored != physical {
		t.Errorf("ledger records %d stored bytes, backend holds %d", stored, physical)
	}
	if stored >= int64(len(body)) {
		t.Errorf("stored %d bytes for a %d byte object, which is no saving at all",
			stored, len(body))
	}
	h.assertEveryCopyDecodesTo(key, body, "after write")
}

// TestCompression_RangeReadsAcrossFrameBoundaries asks for ranges that sit
// inside one frame, span two, land exactly on a boundary, and run to the end of
// the object. Frame-relative offset arithmetic is the part of a chunked format
// that is easy to get subtly wrong, and a range that silently returns the wrong
// window is not something a full-object read would ever reveal.
func TestCompression_RangeReadsAcrossFrameBoundaries(t *testing.T) {
	h := newHarness(t, harnessSpec{Compression: compressionOn()})

	key := uniqueKey(t, "comp-range")
	body := partlyCompressible(compTestChunk * 4)
	h.put(key, body)
	if h.compressionAlgorithm(key) == "" {
		t.Fatalf("fixture was stored verbatim; the ranged decode path never runs")
	}

	assertRanges(t, h, key, body)
}

// assertRanges reads a spread of ranges over an object and requires each to
// return exactly the corresponding window of the bytes written.
func assertRanges(t *testing.T, h *harness, key string, body []byte) {
	t.Helper()
	last := len(body) - 1
	cases := []struct {
		name       string
		start, end int
	}{
		{"first byte", 0, 0},
		{"head of the first frame", 0, 63},
		{"inside the first frame", 100, 4095},
		{"up to the first frame boundary", 0, compTestChunk - 1},
		{"across one frame boundary", compTestChunk - 10, compTestChunk + 10},
		{"exactly the second frame", compTestChunk, compTestChunk*2 - 1},
		{"spanning three frames", compTestChunk / 2, compTestChunk*3 - 1},
		{"tail to the end", compTestChunk * 3, last},
		{"last byte", last, last},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := h.getRange(key, fmt.Sprintf("bytes=%d-%d", tc.start, tc.end))
			want := body[tc.start : tc.end+1]
			if !bytes.Equal(got, want) {
				t.Errorf("range %d-%d returned %d bytes, want %d", tc.start, tc.end, len(got), len(want))
			}
		})
	}
}

// -------------------------------------------------------------------------
// COMPRESSED AND ENCRYPTED
// -------------------------------------------------------------------------

// TestCompression_EncryptedRangeReadsServeCorrectBytes is the combination with
// the most ways to be wrong. The layers compose in one order and only one -
// compress, then encrypt, because ciphertext does not compress - which makes
// the encoded stream the encryptor's plaintext domain and means a ranged read
// translates twice: a logical range into a frame range, and that into a
// ciphertext range.
//
// Getting either translation wrong returns the wrong window rather than an
// error, so the only assertion worth making is on the bytes themselves.
func TestCompression_EncryptedRangeReadsServeCorrectBytes(t *testing.T) {
	h := newHarness(t, harnessSpec{Compression: compressionOn(), Encrypt: true})

	key := uniqueKey(t, "comp-enc-range")
	body := partlyCompressible(compTestChunk * 4)
	target := h.put(key, body)

	if algorithm := h.compressionAlgorithm(key); algorithm != compression.Algorithm {
		t.Fatalf("stored with algorithm %q, want %q", algorithm, compression.Algorithm)
	}
	// Both layers have to be present on the backend, not just recorded. An
	// object stored as a bare encoding would pass every read assertion below
	// while being unencrypted at rest.
	stored := h.storedBytes(target, key)
	if !encryption.HasEnvelopeMagic(stored) {
		t.Fatalf("stored bytes are not an encryption envelope")
	}
	if bytes.Equal(stored, body) {
		t.Fatalf("stored bytes are the client's plaintext")
	}

	if got := h.get(key); !bytes.Equal(got, body) {
		t.Fatalf("full read returned %d bytes, want the %d written", len(got), len(body))
	}
	assertRanges(t, h, key, body)
}

// -------------------------------------------------------------------------
// SCRUBBER
// -------------------------------------------------------------------------

// TestCompression_ScrubberDetectsCorruptedCompressedCopy is the detector the
// feature needs most. A compressed copy is opaque: the orchestrator cannot tell
// good stored bytes from bad by looking at them, and a client cannot either,
// because a GET fails over to the healthy replica and returns the right answer
// while the bad copy sits there.
//
// The hash the scrub compares against covers the bytes the client wrote, so
// verifying a compressed copy means decoding it first. A scrubber that hashed
// the stored form instead would record a digest of the encoding and never
// notice the difference.
func TestCompression_ScrubberDetectsCorruptedCompressedCopy(t *testing.T) {
	h := newHarness(t, harnessSpec{
		Compression: compressionOn(),
		Integrity:   &config.IntegrityConfig{Enabled: true},
	})
	ctx := context.Background()

	key := uniqueKey(t, "comp-scrub")
	body := compressible(compTestChunk * 4)
	h.put(key, body)
	if h.compressionAlgorithm(key) == "" {
		t.Fatalf("fixture was stored verbatim; this is not a compressed scrub")
	}

	// Two copies, so the corrupted one has a healthy sibling to hide behind.
	h.replicate(2)
	backends := h.objectBackends(key)
	if len(backends) != 2 {
		t.Fatalf("expected 2 copies before corrupting one, got %v", backends)
	}
	h.assertEveryCopyDecodesTo(key, body, "after replication")

	// Integrity is on for this harness, so the write path hashed the object and
	// the replica inherited it; backfill runs anyway to cover a copy that
	// somehow arrived without one, and finding nothing to do is a pass. What
	// the scrub actually needs is a stored hash on both copies.
	h.workers.Scrubber.Backfill(ctx, 100, 0, nil)
	if hashed := h.hashedCopies(key); hashed != 2 {
		t.Fatalf("expected 2 copies with a stored hash, got %d", hashed)
	}

	// Replaced with a well-formed encoding of different bytes. That is the
	// corruption a compressed copy has to be checked for by decoding it: the
	// stored bytes are a valid object in their own right, so nothing short of
	// hashing what they decode to can tell them from the real one.
	victim := backends[1]
	h.corrupt(victim, key, h.encode(compressible(compTestChunk*4-1)))

	if got := h.get(key); !bytes.Equal(got, body) {
		t.Fatalf("read failover did not serve the healthy copy: got %d bytes, want %d",
			len(got), len(body))
	}

	if sum := h.workers.Scrubber.Scrub(ctx, 100, nil); sum.Failed != 1 {
		t.Errorf("scrub reported %d mismatches, want 1 (%+v)", sum.Failed, sum)
	}

	if remaining := h.objectBackends(key); len(remaining) != 1 || remaining[0] == victim {
		t.Errorf("ledger still lists %v after the copy on %s was found corrupt", remaining, victim)
	}
	h.assertEveryCopyDecodesTo(key, body, "after the corrupt copy was dropped")
}

// -------------------------------------------------------------------------
// MOVING COMPRESSED OBJECTS
// -------------------------------------------------------------------------

// TestCompression_RebalanceMovesCompressedObjectsIntact moves encoded objects
// between backends and reads them back off the destination.
//
// A rebalance streams the stored bytes rather than the object, so an encoded
// copy has to arrive as the same encoding: the destination row keeps pointing
// at a logical size the bytes there must still decode to. Move counts and
// utilisation percentages, which is what the existing rebalance coverage
// asserts on, would be identical if the bytes had been truncated.
func TestCompression_RebalanceMovesCompressedObjectsIntact(t *testing.T) {
	const (
		dense  = "h-dense"
		sparse = "h-sparse"
	)
	// dense starts with no room at all, so the objects under test land on
	// sparse; it is opened up afterwards and filled to a high utilisation, which
	// is the state pack consolidates into.
	h := newHarness(t, harnessSpec{
		Compression: compressionOn(),
		Backends: []harnessBackend{
			{Name: dense, Quota: 1},
			{Name: sparse, Quota: 1 << 20},
		},
	})
	ctx := context.Background()

	bodies := map[string][]byte{}
	for i := range 4 {
		key := uniqueKey(t, fmt.Sprintf("comp-rebal-%d", i))
		body := compressible(compTestChunk * 2)
		bodies[key] = body
		if target := h.put(key, body); target != sparse {
			t.Fatalf("object %d landed on %s, want %s", i, target, sparse)
		}
	}

	// Incompressible, so it is stored verbatim at exactly this size and the
	// utilisation it creates is arithmetic rather than a guess.
	const fillerSize = 8000
	h.setQuota(dense, 9000)
	fillerKey := uniqueKey(t, "comp-rebal-filler")
	if target := h.put(fillerKey, incompressible(fillerSize)); target != dense {
		t.Fatalf("filler landed on %s, want %s", target, dense)
	}
	if used := h.quotaUsed(dense); used != fillerSize {
		t.Fatalf("%s holds %d bytes, want the %d byte filler stored verbatim", dense, used, fillerSize)
	}

	sum, err := h.workers.Rebalancer.Rebalance(ctx, config.RebalanceConfig{
		Enabled:   true,
		Strategy:  "pack",
		BatchSize: 10,
		Threshold: 0,
	}, nil)
	if err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	if sum.Succeeded == 0 {
		t.Fatalf("pack moved nothing out of %s (%+v)", sparse, sum)
	}

	moved := 0
	for key, body := range bodies {
		if h.objectBackend(key) == dense {
			moved++
		}
		h.assertEveryCopyDecodesTo(key, body, "after rebalance")
		if got := h.get(key); !bytes.Equal(got, body) {
			t.Errorf("%s read back %d bytes after rebalance, want %d", key, len(got), len(body))
		}
	}
	if moved == 0 {
		t.Errorf("no object reached %s, so nothing was byte-verified across a move", dense)
	}
}

// TestCompression_DrainMovesCompressedObjectsIntact evacuates a backend holding
// encoded objects and reads every one of them back off the destination.
//
// Drain relocates every copy on a backend and then deletes the source data, so
// unlike a rebalance there is no original left to fall back on. If the encoding
// does not survive the move the object is simply gone, and the ledger will say
// it is fine.
func TestCompression_DrainMovesCompressedObjectsIntact(t *testing.T) {
	const (
		source = "h-drain-src"
		target = "h-drain-dst"
	)
	h := newHarness(t, harnessSpec{
		Compression: compressionOn(),
		Backends: []harnessBackend{
			{Name: source, Quota: 1 << 20},
			{Name: target, Quota: 1 << 20},
		},
	})
	ctx := context.Background()

	bodies := map[string][]byte{}
	for i := range 4 {
		key := uniqueKey(t, fmt.Sprintf("comp-drain-%d", i))
		body := compressible(compTestChunk * 2)
		bodies[key] = body
		if landed := h.put(key, body); landed != source {
			t.Fatalf("object %d landed on %s, want %s", i, landed, source)
		}
	}
	for key, body := range bodies {
		h.assertEveryCopyDecodesTo(key, body, "before drain")
	}

	if err := h.mgr.Drain().StartDrain(ctx, source); err != nil {
		t.Fatalf("StartDrain(%s): %v", source, err)
	}
	h.waitDrainComplete(source, 60*time.Second)

	if remaining := h.locationsOn(source); remaining != 0 {
		t.Errorf("%s still has %d rows after drain", source, remaining)
	}
	for key, body := range bodies {
		if landed := h.objectBackend(key); landed != target {
			t.Errorf("%s is on %s after drain, want %s", key, landed, target)
		}
		h.assertEveryCopyDecodesTo(key, body, "after drain")
		if got := h.get(key); !bytes.Equal(got, body) {
			t.Errorf("%s read back %d bytes after drain, want %d", key, len(got), len(body))
		}
	}
}
