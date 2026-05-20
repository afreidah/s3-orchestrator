// -------------------------------------------------------------------------------
// Materialized Body - Memory-or-Tempfile Seekable Source
//
// Author: Alex Freidah
//
// Materializes an incoming stream into a seekable form (memory below
// materializeMemThreshold, self-unlinking tempfile above) so PutObject
// failover and CopyObject source replay can re-read the body without
// scaling heap with object size. Reader-reset and lifecycle semantics
// are documented on the relevant methods below.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
)

// materializeMemThreshold is the largest payload size kept entirely in
// memory before the sink spills to a tempfile. Sized to match the AWS
// SDK's own internal heuristic for the PUT signing path. Not a config
// knob because the choice is an implementation detail, not an operator
// concern.
const materializeMemThreshold = 32 * 1024 * 1024

// materializedBody holds a payload that has been buffered into memory
// or onto disk, and serves io.Readers positioned at offset 0 on each
// call. The caller invokes Cleanup once the payload is no longer needed
// (always safe to defer, even on materialization error).
type materializedBody struct {
	buf  *bytes.Buffer
	file *os.File
	size int64
}

// newMaterializedBody copies src into a memory buffer or a tempfile
// based on size, optionally tee'ing the bytes into hasher so the caller
// can compute a content hash in the same single pass instead of
// re-scanning the materialized body afterwards. Pass hasher=nil when
// hashing is not required. The caller must defer (*materializedBody).
// Cleanup on the returned body so the tempfile fd is released when
// the body is no longer needed (safe even on the in-memory branch).
func newMaterializedBody(src io.Reader, size int64, hasher hash.Hash) (*materializedBody, error) {
	mb, err := emptyMaterializedBody(size)
	if err != nil {
		return nil, err
	}
	w := mb.writer()
	if hasher != nil {
		w = io.MultiWriter(w, hasher)
	}
	n, err := bufpool.Copy(w, src)
	if err != nil {
		mb.Cleanup()
		return nil, err
	}
	mb.size = n
	return mb, nil
}

// emptyMaterializedBody allocates the underlying sink without writing
// any bytes. Exposed for code paths that want to drive the write
// themselves (e.g., materializing a foreign GetObject body where the
// integrity hashing is owned by a different layer).
func emptyMaterializedBody(size int64) (*materializedBody, error) {
	if size <= materializeMemThreshold {
		return &materializedBody{buf: &bytes.Buffer{}}, nil
	}
	f, err := os.CreateTemp("", "s3o-put-*")
	if err != nil {
		return nil, fmt.Errorf("create materialize tempfile: %w", err)
	}
	// Unlink immediately so the file disappears on Close or process exit;
	// Cleanup only needs to Close the fd. Removes the leak window if the
	// process panics mid-write.
	_ = os.Remove(f.Name()) //nolint:gosec // G703: path comes from os.CreateTemp, not user input
	return &materializedBody{file: f}, nil
}

// Cleanup releases the underlying tempfile when the sink spilled to
// disk; the in-memory branch has no fd to release and the buffer is
// reclaimed by the GC when the materializedBody goes out of scope.
// Always safe to defer regardless of which branch backs the body.
func (m *materializedBody) Cleanup() {
	if m.file != nil {
		_ = m.file.Close()
	}
}

// writer returns the io.Writer the caller streams source bytes into.
// Only meaningful when emptyMaterializedBody was used to construct the
// sink; newMaterializedBody owns its own write loop.
func (m *materializedBody) writer() io.Writer {
	if m.file != nil {
		return m.file
	}
	return m.buf
}

// Size returns the number of bytes written to the sink. For the
// in-memory sink this is len(buf); for the tempfile sink this is the
// byte count returned by the copy.
func (m *materializedBody) Size() int64 {
	if m.file != nil {
		return m.size
	}
	return int64(m.buf.Len())
}

// Reader returns a fresh io.ReadSeeker positioned at offset 0. Safe to
// call repeatedly so failover attempts and encryption layers can each
// consume the body independently. The in-memory variant returns a
// fresh *bytes.Reader; the tempfile variant rewinds the underlying
// file before returning.
func (m *materializedBody) Reader() (io.ReadSeeker, error) {
	if m.file != nil {
		if _, err := m.file.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("rewind materialize tempfile: %w", err)
		}
		return m.file, nil
	}
	return bytes.NewReader(m.buf.Bytes()), nil
}

// hashHex computes the SHA-256 of buf and returns it as a hex string.
// Convenience for call sites that materialize an in-memory body without
// a streaming hasher attached (i.e., used the empty-sink path).
func sha256Hex(h hash.Hash) string {
	if h == nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// newSHA256 returns a fresh SHA-256 hasher. Exposed as a helper so call
// sites do not need to import crypto/sha256 just to feed
// newMaterializedBody's hasher parameter.
func newSHA256() hash.Hash {
	return sha256.New()
}

// -------------------------------------------------------------------------
// COPY SOURCE MATERIALIZATION
// -------------------------------------------------------------------------

// materializedSource bundles the seekable reader handed to PutObject
// with the cleanup the caller must invoke once the upload settles. The
// returned source backend identifies which replica actually served the
// bytes so CopyObject can attribute usage correctly.
type materializedSource struct {
	body          io.ReadSeeker
	sourceBackend string
	cleanup       func()
}

// materializeCopySource reads the source object from the first
// reachable replica into a seekable buffer  -  in-memory for small
// objects, a self-unlinking tempfile for large ones  -  and returns
// it ready for handoff to PutObject. Failover iterates locations in
// order; backend-side errors (including the backend timeout firing
// per #882) are captured and a different replica is tried. When every
// replica fails the most recent underlying error is returned so
// callers can see why - the previous generic "failed to read source"
// string lost the DeadlineExceeded signal entirely. Per-replica GETs
// run under the backend timeout policy so a stalled source cannot
// exceed backend_timeout; a tighter caller deadline still wins.
func (o *Manager) materializeCopySource(
	ctx context.Context,
	sourceKey string,
	size int64,
	locations []core.ObjectLocation,
) (*materializedSource, error) {
	var lastErr error
	for i := range locations {
		ms, err := o.tryMaterializeFromLocation(ctx, sourceKey, size, locations[i].BackendName)
		if err != nil {
			lastErr = err
			continue
		}
		if ms != nil {
			return ms, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("failed to read source from any copy")
}

// tryMaterializeFromLocation attempts to download sourceKey from one
// backend into a fresh seekable buffer. Returns (ms, nil) on success.
// A (nil, nil) return means the replica was skipped without a hard
// error (usage limits hit, backend not registered) - the caller moves
// to the next replica without capturing an error. A (nil, err) return
// is a real failure: the backend GET errored (including the
// backend-timeout context firing per #882) or the buffer-side
// materialization could not proceed. Errors are aggregated by the
// caller so the last underlying failure surfaces when no replica
// succeeds.
func (o *Manager) tryMaterializeFromLocation(
	ctx context.Context,
	sourceKey string,
	size int64,
	backendName string,
) (*materializedSource, error) {
	if !o.core.Usage().WithinLimits(backendName, 1, size, 0) {
		return nil, nil
	}
	be, ok := o.core.Backends()[backendName]
	if !ok {
		return nil, nil
	}

	// Wrap the source GET in the configured backend timeout so a
	// stalled replica cannot block the materialize step past
	// backend_timeout (#882). The same context covers the body
	// drain inside newMaterializedBody because rcancel only fires
	// on function return.
	rctx, rcancel := o.core.WithTimeout(ctx)
	defer rcancel()
	result, err := be.GetObject(rctx, sourceKey, "")
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()

	mb, err := newMaterializedBody(result.Body, size, nil)
	if err != nil {
		return nil, err
	}
	body, err := mb.Reader()
	if err != nil {
		mb.Cleanup()
		return nil, err
	}
	return &materializedSource{
		body:          body,
		sourceBackend: backendName,
		cleanup:       mb.Cleanup,
	}, nil
}
