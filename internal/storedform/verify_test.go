// -------------------------------------------------------------------------------
// Stored Form - Verification Tests
//
// Author: Alex Freidah
//
// Digest, ContentMatches and Verify over bytes a real codec and encryptor
// produced: a reform that reads back as the source's bytes passes, one judged
// against another hash is a counted mismatch, and bytes that cannot be opened
// are a failure of their own rather than a mismatch.
// -------------------------------------------------------------------------------

package storedform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// verifyOp labels this file's integrity checks, apart from every other caller.
const verifyOp = "verify_test"

// reformedSource seeds a compressed copy of plain carrying its real content
// hash, and returns it with its reform to both layers.
func reformedSource(t *testing.T, l *Layers, plain []byte) (*core.ObjectLocation, *Reformed) {
	t.Helper()
	sum := sha256.Sum256(plain)
	stored, form := seed(t, l, plain, core.StoredCompressed)
	form.ContentHash = hex.EncodeToString(sum[:])
	out, err := l.Reform(context.Background(), bytes.NewReader(stored), int64(len(stored)), form, core.StoredLayers, nil)
	if err != nil {
		t.Fatalf("Reform: %v", err)
	}
	t.Cleanup(out.Release)
	return &core.ObjectLocation{ObjectKey: "k", BackendName: "b1", StoredForm: *form}, out
}

// TestVerify_JudgesTheReformAgainstTheSourceHash verifies a reform that reads
// back as the source's bytes passes, one judged against another hash is a
// counted mismatch, and a source with no hash is not checked.
func TestVerify_JudgesTheReformAgainstTheSourceHash(t *testing.T) {
	t.Parallel()
	l := newLayers(t)
	src, out := reformedSource(t, l, compressible())
	log := slog.New(slog.DiscardHandler)

	if err := l.Verify(context.Background(), log, verifyOp, src, out); err != nil {
		t.Errorf("matching reform: %v", err)
	}
	other := *src
	other.ContentHash = "0000"
	before := testutil.ToFloat64(telemetry.IntegrityErrorsTotal.WithLabelValues(verifyOp))
	if err := l.Verify(context.Background(), log, verifyOp, &other, out); !errors.Is(err, ErrContentMismatch) {
		t.Errorf("other hash: err = %v, want ErrContentMismatch", err)
	}
	if got := testutil.ToFloat64(telemetry.IntegrityErrorsTotal.WithLabelValues(verifyOp)) - before; got != 1 {
		t.Errorf("integrity errors rose by %v, want 1", got)
	}
	other.ContentHash = ""
	if err := l.Verify(context.Background(), log, verifyOp, &other, out); err != nil {
		t.Errorf("unhashed source: %v", err)
	}
}

// TestVerify_UnreadableReformIsNotAMismatch verifies bytes the layers cannot
// open fail the check with why, rather than reading as a hash mismatch.
func TestVerify_UnreadableReformIsNotAMismatch(t *testing.T) {
	t.Parallel()
	l := newLayers(t)
	src, out := reformedSource(t, l, compressible())
	noCodec := &Layers{Encryptor: l.Encryptor}
	err := noCodec.Verify(context.Background(), slog.New(slog.DiscardHandler), verifyOp, src, out)
	if !errors.Is(err, ErrNoCodec) || errors.Is(err, ErrContentMismatch) {
		t.Errorf("err = %v, want ErrNoCodec and not a mismatch", err)
	}
}

// TestDigest_ReadFailure verifies a body that fails mid-read is reported as a
// read failure.
func TestDigest_ReadFailure(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("connection reset")
	err := Digest(context.Background(), nil, nil, iotest.ErrReader(sentinel), &core.StoredForm{}, sha256.New())
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the read failure", err)
	}
}

// TestReformed_BufferChecksTheSize verifies a last stage that does not come to
// the size it reported fails the reform rather than buffering short bytes.
func TestReformed_BufferChecksTheSize(t *testing.T) {
	t.Parallel()
	out := &Reformed{}
	if err := out.buffer(strings.NewReader("abc"), 5); err == nil {
		t.Error("buffer accepted 3 bytes reported as 5")
	}
	if out.Body != nil {
		t.Error("a failed buffer left a body behind")
	}
}
