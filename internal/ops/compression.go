// -------------------------------------------------------------------------------
// Ops - Compression Operations
//
// Author: Alex Freidah
//
// Fleet-wide transitions between stored-verbatim and stored-encoded. Enabling
// compression only affects objects written afterwards, so these are what make
// the feature adoptable on a fleet that already holds data - and what takes it
// back out again.
//
// Both directions drive the same pagination, download, transform, re-upload and
// metadata-update loop the encryption passes use, differing only in the listing
// query and the transform. The transform is where compression is harder than
// encryption: it sits inside encryption, so an encrypted copy has to be
// decrypted before its bytes can be encoded and re-encrypted afterwards.
// -------------------------------------------------------------------------------

package ops

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/materialize"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// CompressionDeps holds the collaborators Compression requires.
type CompressionDeps struct {
	Codec     CompressionCodec
	Config    config.CompressionConfig
	Encryptor *encryption.Encryptor
	Store     CompressionStore
	Runtime   RuntimeOps
	Usage     UsageGate
}

// Compression serves the fleet-wide compression operations. Codec and Store are
// nil when the orchestrator was started without them, which every operation
// reports as ErrCompressionUnavailable.
//
// Encryptor may be nil, in which case encrypted copies are refused.
type Compression struct {
	log       *slog.Logger
	codec     CompressionCodec
	cfg       config.CompressionConfig
	encryptor *encryption.Encryptor
	store     CompressionStore
	runtime   RuntimeOps
	usage     UsageGate
}

// NewCompression is the explicit-deps constructor.
func NewCompression(d *CompressionDeps) *Compression {
	must.NotNil("d.Runtime", d.Runtime)
	must.NotNil("d.Usage", d.Usage)
	return &Compression{
		log:       slog.Default().With(logfmt.Component("ops")),
		codec:     d.Codec,
		cfg:       d.Config,
		encryptor: d.Encryptor,
		store:     d.Store,
		runtime:   d.Runtime,
		usage:     d.Usage,
	}
}

// rewriteEnv exposes this service's collaborators to the shared driver.
func (c *Compression) rewriteEnv() bulkRewriteEnv {
	return bulkRewriteEnv{log: c.log, runtime: c.runtime, usage: c.usage}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// CompressExisting encodes every copy stored verbatim and records the new
// stored form, applying the same thresholds a PUT does. Objects that do not
// shrink past the configured ratio are left as they are and counted as skipped.
//
// maxRewrites caps how many copies are rewritten, or zero for the whole fleet.
// Rewritten and declined copies both leave the listing, so running a capped
// pass again continues with the next batch.
func (c *Compression) CompressExisting(ctx context.Context, obs progress.Observer, maxRewrites int, backend string) (BulkRewriteResult, error) {
	if c.codec == nil || c.store == nil {
		return BulkRewriteResult{}, ErrCompressionUnavailable
	}
	return bulkRewriteOp[*rewriteRow]{
		opName:      "compress-existing",
		resultLabel: "compressed",
		counter:     telemetry.CompressExistingObjectsTotal,
		// The size floor and the recorded declines are both applied by the
		// listing rather than here: both answers outlive the pass, so a copy
		// either one excludes selects out of every future pass instead of being
		// handed to each one only to be declined again.
		listFn: rewriteListFn(func(ctx context.Context, batchSize int, after core.Cursor) ([]core.RewritableLocation, error) {
			return c.store.ListUncompressedLocations(ctx, batchSize, after, core.CompressionThresholds{
				MinSize:  c.cfg.MinSize,
				MinRatio: c.cfg.MinRatio,
				Level:    c.cfg.Level,
			}, backend)
		}),
		rewrite:     c.compressOne,
		maxRewrites: maxRewrites,
	}.run(ctx, c.rewriteEnv(), obs)
}

// DecompressExisting decodes every encoded copy and records it as stored
// verbatim. maxRewrites caps how many copies are rewritten, or zero for the
// whole fleet; a capped run can be repeated to continue.
func (c *Compression) DecompressExisting(ctx context.Context, obs progress.Observer, maxRewrites int, backend string) (BulkRewriteResult, error) {
	if c.codec == nil || c.store == nil {
		return BulkRewriteResult{}, ErrCompressionUnavailable
	}
	return bulkRewriteOp[*rewriteRow]{
		opName:      "decompress-existing",
		resultLabel: "decompressed",
		counter:     telemetry.DecompressExistingObjectsTotal,
		listFn: rewriteListFn(func(ctx context.Context, batchSize int, after core.Cursor) ([]core.RewritableLocation, error) {
			return c.store.ListCompressedLocations(ctx, batchSize, after, backend)
		}),
		rewrite:     c.decompressOne,
		maxRewrites: maxRewrites,
	}.run(ctx, c.rewriteEnv(), obs)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// compressOne encodes one copy. The encoded bytes are buffered because a PUT
// declares its size up front and an encoder only knows that size at the end,
// which is also what lets the ratio be judged before anything is written.
func (c *Compression) compressOne(ctx context.Context, src *s3be.GetObjectResult, loc *rewriteRow) (rewritten, error) {
	logical := loc.LogicalSizeOfSource()
	plain, err := c.plaintextOf(ctx, src.Body, loc)
	if err != nil {
		return rewritten{}, err
	}

	encoded, err := materialize.NewEmpty(logical)
	if err != nil {
		return rewritten{}, fmt.Errorf("buffer encoded object: %w", err)
	}
	encodedSize, err := c.codec.Compress(encoded.Writer(), plain)
	if err != nil {
		encoded.Cleanup()
		telemetry.CompressionErrorsTotal.WithLabelValues(telemetry.CompressionOpEncode).Inc()
		return rewritten{}, fmt.Errorf("compress: %w", err)
	}
	if !compression.WorthStoring(logical, encodedSize, c.cfg.MinRatio) {
		encoded.Cleanup()
		telemetry.CompressionSkippedTotal.WithLabelValues(telemetry.CompressionSkipMinRatio).Inc()
		// Record the decline so later runs skip this copy instead of
		// re-encoding it. A failure here costs efficiency, not correctness.
		if err := c.store.RecordCompressionProbe(ctx, &core.CompressionProbe{
			ObjectKey:   loc.ObjectKey,
			BackendName: loc.BackendName,
			Size:        encodedSize,
			Level:       c.cfg.Level,
		}); err != nil {
			c.log.WarnContext(ctx, "failed to record compression probe",
				"key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
		}
		return rewritten{}, errSkipRewrite
	}
	telemetry.RecordCompressed(logical, encodedSize)
	body, err := encoded.Reader()
	if err != nil {
		encoded.Cleanup()
		return rewritten{}, fmt.Errorf("read back encoded object: %w", err)
	}

	out, err := c.seal(ctx, body, encodedSize, loc)
	if err != nil {
		encoded.Cleanup()
		return rewritten{}, err
	}
	update := core.CompressedUpdate{
		ObjectKey:     loc.ObjectKey,
		BackendName:   loc.BackendName,
		Algorithm:     compression.Algorithm,
		Level:         c.cfg.Level,
		FormatVersion: compression.FormatVersion,
		SizeBytes:     out.size,
		PlaintextSize: out.inner,
		LogicalSize:   logical,
		EncryptionKey: out.key,
		KeyID:         out.keyID,
		ExpectedEtag:  loc.Etag,
	}
	previous := loc.SizeBytes
	return rewritten{
		body:    out.body,
		size:    out.size,
		commit:  func() error { return c.store.MarkObjectCompressed(ctx, &update, previous) },
		release: encoded.Cleanup,
	}, nil
}

// decompressOne decodes one copy back to the bytes the client wrote. The
// decoded size is known from the row, so unlike the encode direction this
// streams: a decoder is handed a size rather than discovering one.
func (c *Compression) decompressOne(ctx context.Context, src *s3be.GetObjectResult, loc *rewriteRow) (rewritten, error) {
	stored, err := c.plaintextOf(ctx, src.Body, loc)
	if err != nil {
		return rewritten{}, err
	}
	decoded, err := c.codec.DecompressStream(stored)
	if err != nil {
		telemetry.CompressionErrorsTotal.WithLabelValues(telemetry.CompressionOpDecode).Inc()
		return rewritten{}, fmt.Errorf("decompress: %w", err)
	}

	out, err := c.seal(ctx, decoded, loc.LogicalSize, loc)
	if err != nil {
		_ = decoded.Close()
		return rewritten{}, err
	}
	update := core.CompressedUpdate{
		ObjectKey:     loc.ObjectKey,
		BackendName:   loc.BackendName,
		SizeBytes:     out.size,
		PlaintextSize: out.inner,
		EncryptionKey: out.key,
		KeyID:         out.keyID,
		ExpectedEtag:  loc.Etag,
	}
	previous := loc.SizeBytes
	return rewritten{
		body:    out.body,
		size:    out.size,
		commit:  func() error { return c.store.MarkObjectCompressed(ctx, &update, previous) },
		release: func() { _ = decoded.Close() },
	}, nil
}

// plaintextOf unwraps a copy's encryption, if it has any, so the transform sees
// the bytes compression actually operates on. A copy recorded as encrypted with
// no encryptor configured is refused rather than rewritten as though it were
// plaintext, which would publish ciphertext as the object.
func (c *Compression) plaintextOf(ctx context.Context, body io.Reader, loc *rewriteRow) (io.Reader, error) {
	if !loc.Encrypted {
		return body, nil
	}
	if c.encryptor == nil {
		return nil, ErrEncryptionDisabled
	}
	plain, _, err := c.encryptor.DecryptStored(ctx, body, loc.EncryptionKey, loc.KeyID, loc.PlaintextSize, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plain, nil
}

// sealedBody is a rewritten body ready for upload: what to send, how many bytes
// that is, and - when the copy was encrypted - the description of the envelope
// it was wrapped in. The row must be updated with key and keyID, since the old
// nonce and wrapped key cannot decrypt the re-encrypted bytes.
type sealedBody struct {
	body  io.Reader
	size  int64
	inner int64
	key   []byte
	keyID string
}

// seal re-applies encryption to a rewritten body when the copy was encrypted,
// minting a fresh data key wrapped under the current primary. It assumes an
// encryptor is configured, since plaintextOf already refused an encrypted copy
// without one.
func (c *Compression) seal(ctx context.Context, body io.Reader, size int64, loc *rewriteRow) (sealedBody, error) {
	if !loc.Encrypted {
		return sealedBody{body: body, size: size}, nil
	}
	res, err := c.encryptor.Encrypt(ctx, body, size)
	if err != nil {
		return sealedBody{}, fmt.Errorf("re-encrypt: %w", err)
	}
	return sealedBody{
		body:  res.Body,
		size:  res.CiphertextSize,
		inner: size,
		key:   encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
		keyID: res.KeyID,
	}, nil
}

// rewriteRow adapts a rewritable location to bulkRewriteRow. Pointer receivers
// avoid copying the embedded store row.
type rewriteRow struct{ core.RewritableLocation }

// rewriteKey returns the object key to re-process.
func (r *rewriteRow) rewriteKey() string { return r.ObjectKey }

// rewriteBackend returns the backend the row currently lives on.
func (r *rewriteRow) rewriteBackend() string { return r.BackendName }

// rewriteStorageKey returns the path this copy's bytes occupy on its backend,
// which is what the rewrite reads and writes.
func (r *rewriteRow) rewriteStorageKey() string { return r.StorageKey }

// rewriteSize returns the row's stored size, used for quota accounting.
func (r *rewriteRow) rewriteSize() int64 { return r.SizeBytes }

// rewriteEtag returns what the copy reported when the listing selected it, which
// the commit is predicated on.
func (r *rewriteRow) rewriteEtag() string { return r.Etag }

// LogicalSizeOfSource reports how many bytes the transform will read: the
// object the client wrote. That is logical_size for an encoded copy,
// plaintext_size for an encrypted one, and the stored size otherwise.
func (r *rewriteRow) LogicalSizeOfSource() int64 {
	switch {
	case r.CompressionAlgorithm != "":
		return r.LogicalSize
	case r.Encrypted:
		return r.PlaintextSize
	default:
		return r.SizeBytes
	}
}

// rewriteListFn adapts a store listing to the driver's paging callback.
func rewriteListFn(list func(context.Context, int, core.Cursor) ([]core.RewritableLocation, error)) func(context.Context, int, core.Cursor) ([]*rewriteRow, error) {
	return func(ctx context.Context, batchSize int, after core.Cursor) ([]*rewriteRow, error) {
		rows, err := list(ctx, batchSize, after)
		if err != nil {
			return nil, err
		}
		out := make([]*rewriteRow, len(rows))
		for i := range rows {
			out[i] = &rewriteRow{rows[i]}
		}
		return out, nil
	}
}
