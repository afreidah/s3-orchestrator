// -------------------------------------------------------------------------------
// CopyObject Source Materialization
//
// Author: Alex Freidah
//
// Materializes the source object's body into a seekable reader before
// the destination PutObject runs. CopyObject originally streamed the
// source through io.Pipe, but a non-seekable body forces the AWS SDK
// into STREAMING-UNSIGNED-PAYLOAD-TRAILER signing, which uses HTTP
// chunked transfer encoding and drops Content-Length. S3
// implementations that require Content-Length on PUT (notably OCI)
// then reject the upload with HTTP 411.
//
// Materializing to a seekable buffer keeps the SDK on the non-streaming
// UNSIGNED-PAYLOAD path, preserves Content-Length, and works across
// every supported backend. Objects up to copyMaterializeMemThreshold
// stay in memory; larger ones spill to a tempfile that is unlinked
// immediately after open so a panic or crash cannot leak it.
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/bufpool"
)

// copyMaterializeMemThreshold is the largest source object size that
// materializeCopySource keeps in memory. Above this, the helper spills
// to a tempfile. Mirrors the AWS SDK's own internal heuristic for the
// PUT signing path; not a config knob because copy-vs-tempfile is an
// implementation detail, not an operator concern.
const copyMaterializeMemThreshold = 32 * 1024 * 1024

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
// order and resets the buffer between attempts so a partial read from
// one replica does not contaminate the next try. Returns an error only
// when every replica fails or buffering itself errors. The supplied
// ctx flows unmodified into the backend GetObject calls; the caller
// owns timeout and cancellation policy.
func (o *ObjectManager) materializeCopySource(
	ctx context.Context,
	sourceKey string,
	size int64,
	locations []core.ObjectLocation,
) (*materializedSource, error) {
	for i := range locations {
		ms, ok, err := o.tryMaterializeFromLocation(ctx, sourceKey, size, locations[i].BackendName)
		if err != nil {
			return nil, err
		}
		if ok {
			return ms, nil
		}
	}
	return nil, fmt.Errorf("failed to read source from any copy")
}

// tryMaterializeFromLocation attempts to download sourceKey from one
// backend into a fresh seekable buffer. Returns ok=true when the read
// completed and the buffer is ready for PutObject. ok=false means the
// caller should move on to the next replica; err is reserved for
// buffer-side failures (out of memory, tempfile creation) that cannot
// be retried by switching replicas.
func (o *ObjectManager) tryMaterializeFromLocation(
	ctx context.Context,
	sourceKey string,
	size int64,
	backendName string,
) (*materializedSource, bool, error) {
	if !o.usage.WithinLimits(backendName, 1, size, 0) {
		return nil, false, nil
	}
	be, ok := o.backends[backendName]
	if !ok {
		return nil, false, nil
	}

	result, err := be.GetObject(ctx, sourceKey, "")
	if err != nil {
		return nil, false, nil
	}
	defer result.Body.Close()

	sink, cleanup, err := newCopyMaterializeSink(size)
	if err != nil {
		return nil, false, err
	}
	if _, err := bufpool.Copy(sink.writer(), result.Body); err != nil {
		cleanup()
		return nil, false, nil
	}
	body, err := sink.seekableBody()
	if err != nil {
		cleanup()
		return nil, false, err
	}
	return &materializedSource{
		body:          body,
		sourceBackend: backendName,
		cleanup:       cleanup,
	}, true, nil
}

// copyMaterializeSink hides whether the materialized body lives in
// memory or on disk so tryMaterializeFromLocation can write through a
// uniform interface and only branch on which seekable reader to
// return.
type copyMaterializeSink struct {
	buf  *bytes.Buffer
	file *os.File
}

// newCopyMaterializeSink picks the in-memory or tempfile sink based on
// the known object size. Returns a cleanup that the caller must invoke
// once the upload settles, including on materialization error.
func newCopyMaterializeSink(size int64) (*copyMaterializeSink, func(), error) {
	if size <= copyMaterializeMemThreshold {
		noopCleanup := func() {
			// In-memory sink owns no resource; the buffer is
			// reclaimed by the GC when the materializedSource goes
			// out of scope. Cleanup is a no-op so the caller can
			// always defer it without branching on sink type.
		}
		return &copyMaterializeSink{buf: &bytes.Buffer{}}, noopCleanup, nil
	}
	f, err := os.CreateTemp("", "s3o-copy-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create copy tempfile: %w", err)
	}
	// Unlink immediately so the file disappears on Close or process
	// exit; cleanup() only needs to close the fd. Removes the
	// possibility of leaking a tempfile if the process panics
	// mid-copy.
	_ = os.Remove(f.Name()) //nolint:gosec // G703: path comes from os.CreateTemp, not user input
	return &copyMaterializeSink{file: f}, func() { _ = f.Close() }, nil
}

// writer returns the io.Writer the source body streams into.
func (s *copyMaterializeSink) writer() io.Writer {
	if s.file != nil {
		return s.file
	}
	return s.buf
}

// seekableBody returns the io.ReadSeeker handed to PutObject. For the
// tempfile sink this rewinds the file so PutObject reads from offset 0.
func (s *copyMaterializeSink) seekableBody() (io.ReadSeeker, error) {
	if s.file != nil {
		if _, err := s.file.Seek(0, io.SeekStart); err != nil { //nolint:gosec // G703: file path comes from os.CreateTemp("", ...), not user input
			return nil, fmt.Errorf("rewind copy tempfile: %w", err)
		}
		return s.file, nil
	}
	return bytes.NewReader(s.buf.Bytes()), nil
}
