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

// CompressionDeps holds the collaborators Compression requires.
type CompressionDeps struct {
	Codec      CompressionCodec
	Config     config.CompressionConfig
	Encryptor  *encryption.Encryptor
	Store      CompressionStore
	Runtime    RuntimeOps
	BackendOps BackendOps
}

// Compression serves the fleet-wide compression operations. Codec and Store are
// nil when the orchestrator was started without them, which every operation
// reports as ErrCompressionUnavailable.
//
// Encryptor may be nil: a fleet with no encryption has no encrypted copies to
// rewrite, and one that does is refused rather than rewritten blind.
type Compression struct {
	log        *slog.Logger
	codec      CompressionCodec
	cfg        config.CompressionConfig
	encryptor  *encryption.Encryptor
	store      CompressionStore
	runtime    RuntimeOps
	backendOps BackendOps
}

// NewCompression is the explicit-deps constructor.
func NewCompression(d *CompressionDeps) *Compression {
	must.NotNil("d.Runtime", d.Runtime)
	must.NotNil("d.BackendOps", d.BackendOps)
	return &Compression{
		log:        slog.Default().With(logfmt.Component("ops")),
		codec:      d.Codec,
		cfg:        d.Config,
		encryptor:  d.Encryptor,
		store:      d.Store,
		runtime:    d.Runtime,
		backendOps: d.BackendOps,
	}
}

// rewriteEnv exposes this service's collaborators to the shared driver.
func (c *Compression) rewriteEnv() bulkRewriteEnv {
	return bulkRewriteEnv{log: c.log, runtime: c.runtime, backendOps: c.backendOps}
}

// CompressExisting encodes every copy stored verbatim and records the new
// stored form. Objects the encoder cannot shrink past the configured ratio are
// left exactly as they are and counted as skipped: the pass applies the same
// thresholds a PUT does, so it cannot write an encoding a fresh write would
// have rejected.
//
// maxRewrites caps how many copies are rewritten, or zero for the whole fleet. A
// capped run needs nothing carried between invocations to continue: a rewritten
// copy leaves the listing, and one declined on ratio is recorded so it leaves too,
// so running it again converts the next batch rather than re-examining the last.
func (c *Compression) CompressExisting(ctx context.Context, obs progress.Observer, maxRewrites int) (BulkRewriteResult, error) {
	if c.codec == nil || c.store == nil {
		return BulkRewriteResult{}, ErrCompressionUnavailable
	}
	return runBulkRewrite(c.rewriteEnv(), ctx, obs, bulkRewriteOp[*rewriteRow]{
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
			})
		}),
		rewrite:     c.compressOne,
		maxRewrites: maxRewrites,
	})
}

// DecompressExisting decodes every encoded copy and records it as stored
// verbatim, which is what an operator runs to take the feature back out.
//
// maxRewrites caps how many copies are rewritten, or zero for the whole fleet.
// This direction declines nothing, so every copy a capped run touches leaves the
// listing and the next run continues straight on from there.
func (c *Compression) DecompressExisting(ctx context.Context, obs progress.Observer, maxRewrites int) (BulkRewriteResult, error) {
	if c.codec == nil || c.store == nil {
		return BulkRewriteResult{}, ErrCompressionUnavailable
	}
	return runBulkRewrite(c.rewriteEnv(), ctx, obs, bulkRewriteOp[*rewriteRow]{
		opName:      "decompress-existing",
		resultLabel: "decompressed",
		counter:     telemetry.DecompressExistingObjectsTotal,
		listFn:      rewriteListFn(c.store.ListCompressedLocations),
		rewrite:     c.decompressOne,
		maxRewrites: maxRewrites,
	})
}

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
		// What the encode cost bought is the knowledge that this copy does not
		// shrink enough, so it is written down. A pass that discarded it would
		// spend the same download and encode to learn it again on every run.
		// The failure is logged and swallowed: the copy is correctly declined
		// either way, and losing the record costs efficiency, not correctness.
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
// it was wrapped in.
//
// key and keyID are what make the rewrite survivable. Re-encrypting produces a
// new base nonce and a new wrapped data key, so a row left holding the old ones
// describes bytes nothing can decrypt.
type sealedBody struct {
	body  io.Reader
	size  int64
	inner int64
	key   []byte
	keyID string
}

// seal re-applies encryption to a rewritten body when the copy was encrypted. A
// fresh data key is minted rather than the old one reused: re-encryption
// changes the base nonce whatever key is used, so the row has to be updated
// either way, and the new key is wrapped under whichever is primary now.
// A missing encryptor needs no check here: plaintextOf runs first in both
// transforms and refuses an encrypted copy it cannot unwrap, so this is only
// ever reached with one configured.
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

// rewriteSize returns the row's stored size, used for quota accounting.
func (r *rewriteRow) rewriteSize() int64 { return r.SizeBytes }

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
