// -------------------------------------------------------------------------------
// Stored Form - Encoding Plaintext for a Backend and Decoding It Back
//
// Author: Alex Freidah
//
// What a copy's row says about its bytes - compressed, encrypted, both or
// neither - is produced here on the way in and undone here on the way out. The
// layers always stack the same way: compression runs on the client's bytes and
// encryption on whatever compression produced, since ciphertext does not
// compress. Every path that writes or reads a whole object goes through these
// stages, so a write, a rewrite pass and a verification read cannot disagree
// about what a row's columns mean.
//
// Ranged reads are the exception. They decode only the frames a range covers,
// through the seek table, and stay with the read path.
// -------------------------------------------------------------------------------

package storedform

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/materialize"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// encryptOp labels the encrypt side of the encryption metrics.
const encryptOp = "encrypt"

// ErrNoCodec and ErrNoEncryptor report a copy whose stored form needs a layer
// this orchestrator is not configured to undo or apply. Such a copy cannot be
// judged, so callers treat it as unreadable rather than corrupt: reading
// ciphertext or an encoding as the client's bytes would condemn a copy that
// was never damaged.
var (
	ErrNoCodec     = errors.New("object is compressed but no codec is configured")
	ErrNoEncryptor = errors.New("object is encrypted but no encryptor is configured")
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Decompressor decodes a stored object front to back, which is all a whole
// read needs. Declared here rather than taking *compression.Codec so a test can
// present bytes that will not decode without hand-building a corrupt object.
type Decompressor interface {
	DecompressStream(r io.Reader) (io.ReadCloser, error)
}

// Encoder compresses a stream, which is all a write needs.
type Encoder interface {
	Compress(dst io.Writer, src io.Reader) (int64, error)
}

// Codec encodes as well as decodes, which is what a rewrite pass needs.
// *compression.Codec satisfies it.
type Codec interface {
	Decompressor
	Encoder
}

// Encoding is one compression attempt. Body holds the encoded bytes and Size
// how many there are. Worth reports whether the encoding met the ratio; when
// it did not, the caller still owns Body and decides whether to discard it or
// decode the plaintext back out of it.
type Encoding struct {
	Body  *materialize.Body
	Size  int64
	Worth bool
}

// DataKey is an already wrapped data key a caller seals under instead of
// minting one, as a multipart upload does so its parts and the assembled object
// share one key.
type DataKey struct {
	DEK        []byte
	WrappedDEK []byte
	KeyID      string
}

// Sealed is a body wrapped in an encryption envelope, its size, and the form
// describing the envelope. The caller adds what it knows about the layers
// underneath, such as compression and the content hash.
type Sealed struct {
	Body io.Reader
	Size int64
	Form *core.StoredForm
}

// Layers is what a rewrite needs to take a copy from one stored form to
// another: the encryptor and codec, either of which may be nil when it is not
// configured, and the compression settings an encoding is judged by.
type Layers struct {
	Encryptor   *encryption.Encryptor
	Codec       Codec
	Compression config.CompressionConfig
}

// Reformed is a copy's bytes in a new stored form: what to send, how many bytes
// that is, and the form describing them. Release frees anything the reform
// buffered and must run whether or not the upload succeeds.
type Reformed struct {
	Body    io.Reader
	Size    int64
	Form    *core.StoredForm
	release []func()
}

// Declined reports a reform whose encoding did not meet the configured ratio,
// with the size the encoding came to, so the caller can record the decline.
type Declined struct {
	EncodedSize int64
}

// streamMetricReader counts non-EOF errors surfaced mid-stream, such as a
// backend reset during a copy, through the same counter constructor-time
// encryption failures use.
type streamMetricReader struct {
	io.Reader
	op string
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Error reports the decline.
func (d *Declined) Error() string {
	return fmt.Sprintf("encoding of %d bytes did not meet the compression ratio", d.EncodedSize)
}

// Release frees what the reform buffered. Safe to call more than once.
func (r *Reformed) Release() {
	for _, fn := range r.release {
		fn()
	}
	r.release = nil
}

// Read forwards to the wrapped reader and increments the encryption errors
// counter when a non-EOF error surfaces.
func (m *streamMetricReader) Read(p []byte) (int, error) {
	n, err := m.Reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		telemetry.EncryptionErrorsTotal.WithLabelValues(m.op, "stream_failed").Inc()
	}
	return n, err
}

// WithStreamMetric wraps r so a non-EOF read error increments the encryption
// errors counter under op. Callers keep any io.Closer they hold alongside r.
func WithStreamMetric(r io.Reader, op string) io.Reader {
	return &streamMetricReader{Reader: r, op: op}
}

// ShouldCompress reports whether a write of size bytes is encoded under cfg.
// Objects under the minimum are stored verbatim, since a seek table and frame
// headers cost more than a small object saves; that decline is counted.
func ShouldCompress(cfg *config.CompressionConfig, codec Encoder, size int64) bool {
	if codec == nil || !cfg.Enabled {
		return false
	}
	if size < cfg.MinSize {
		telemetry.CompressionSkippedTotal.WithLabelValues(telemetry.CompressionSkipMinSize).Inc()
		return false
	}
	return true
}

// Compress encodes src into a buffer of its own and judges the result against
// minRatio. The encoding is buffered because a backend PUT declares its size
// up front and an encoder only knows that size at the end. A miss is counted
// and handed back with Worth false; the caller releases Body either way.
func Compress(codec Encoder, src io.Reader, logicalSize int64, minRatio float64) (*Encoding, error) {
	// Sized by the logical size, since the encoded form is never larger by
	// enough to matter.
	buf, err := materialize.NewEmpty(logicalSize)
	if err != nil {
		return nil, fmt.Errorf("buffer encoded object: %w", err)
	}
	n, err := codec.Compress(buf.Writer(), src)
	if err != nil {
		buf.Cleanup()
		telemetry.CompressionErrorsTotal.WithLabelValues(telemetry.CompressionOpEncode).Inc()
		return nil, fmt.Errorf("compress: %w", err)
	}
	worth := compression.WorthStoring(logicalSize, n, minRatio)
	if worth {
		telemetry.RecordCompressed(logicalSize, n)
	} else {
		telemetry.CompressionSkippedTotal.WithLabelValues(telemetry.CompressionSkipMinRatio).Inc()
	}
	return &Encoding{Body: buf, Size: n, Worth: worth}, nil
}

// Describe records on form how the encoding was made, allocating a form when
// the caller has none yet, and returns it. LogicalSize is the only place the
// client-visible size survives, since the row's SizeBytes counts the encoding.
func Describe(form *core.StoredForm, level string, logicalSize int64) *core.StoredForm {
	if form == nil {
		form = &core.StoredForm{}
	}
	form.CompressionAlgorithm = compression.Algorithm
	form.CompressionLevel = level
	form.CompressionFormatVersion = compression.FormatVersion
	form.LogicalSize = logicalSize
	return form
}

// Seal wraps body in an encryption envelope under key, or under a freshly
// minted data key when key is nil. The returned body streams; a caller that
// sends it more than once materializes it, since a second seal would draw a
// new base nonce and the copies would not hold the same bytes.
func Seal(ctx context.Context, enc *encryption.Encryptor, body io.Reader, plaintextSize int64, key *DataKey) (*Sealed, error) {
	var (
		res *encryption.EncryptResult
		err error
	)
	if key != nil {
		res, err = enc.EncryptWithDEK(body, plaintextSize, key.DEK, key.WrappedDEK, key.KeyID)
	} else {
		res, err = enc.Encrypt(ctx, body, plaintextSize)
	}
	if err != nil {
		telemetry.EncryptionErrorsTotal.WithLabelValues(encryptOp, "encrypt_failed").Inc()
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	telemetry.EncryptionOpsTotal.WithLabelValues(encryptOp).Inc()
	return &Sealed{
		Body: WithStreamMetric(res.Body, encryptOp),
		Size: res.CiphertextSize,
		Form: &core.StoredForm{
			Encrypted:     true,
			EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
			KeyID:         res.KeyID,
			PlaintextSize: plaintextSize,
		},
	}, nil
}

// CheckEnvelope compares the stored bytes against what form claims about
// encryption and returns a reader that still yields them from the start.
// Reading an envelope as plaintext would hash or serve ciphertext as the
// object, so a row whose flag disagrees with its bytes is
// ErrEncryptionFlagMismatch.
func CheckEnvelope(body io.Reader, form *core.StoredForm) (io.Reader, error) {
	isEnvelope, body, err := encryption.PeekEnvelope(body)
	if err != nil {
		return nil, fmt.Errorf("inspect object header: %w", err)
	}
	if isEnvelope != form.Encrypted {
		return body, fmt.Errorf("%w: row says encrypted=%t but stored bytes say encrypted=%t",
			core.ErrEncryptionFlagMismatch, form.Encrypted, isEnvelope)
	}
	return body, nil
}

// Unseal checks the stored bytes against form and removes the envelope when
// there is one, returning the bytes compression produced, or the client's
// bytes for an uncompressed copy.
func Unseal(ctx context.Context, enc *encryption.Encryptor, body io.Reader, form *core.StoredForm) (io.Reader, error) {
	body, err := CheckEnvelope(body, form)
	if err != nil {
		return nil, err
	}
	if !form.Encrypted {
		return body, nil
	}
	if enc == nil {
		return nil, ErrNoEncryptor
	}
	plain, _, err := enc.DecryptStored(ctx, body, form.EncryptionKey, form.KeyID, form.PlaintextSize, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plain, nil
}

// Decode removes the compression layer form records, front to back rather than
// through the seek table, since the whole object is read anyway. The returned
// closer is always safe to call.
func Decode(codec Decompressor, r io.Reader, form *core.StoredForm) (io.Reader, func(), error) {
	if !form.Form().Has(core.StoredCompressed) {
		return r, noop, nil
	}
	if codec == nil {
		return nil, noop, ErrNoCodec
	}
	decoded, err := codec.DecompressStream(r)
	if err != nil {
		telemetry.CompressionErrorsTotal.WithLabelValues(telemetry.CompressionOpDecode).Inc()
		return nil, noop, fmt.Errorf("decompress: %w", err)
	}
	return decoded, func() { _ = decoded.Close() }, nil
}

// Open undoes the whole stored form in the order it was applied, decrypt then
// decompress, and returns the bytes the client wrote. The returned closer is
// always safe to call.
func Open(ctx context.Context, enc *encryption.Encryptor, codec Decompressor, body io.Reader, form *core.StoredForm) (io.Reader, func(), error) {
	unsealed, err := Unseal(ctx, enc, body, form)
	if err != nil {
		return nil, noop, err
	}
	return Decode(codec, unsealed, form)
}

// Reform rewrites a copy's stored bytes from the form from describes into one
// carrying the layers in want, touching only the layers that change: an
// encoding that stays is passed through rather than decoded and re-encoded.
// storedSize is how many bytes body holds. The content hash carries over,
// since it covers the bytes the client wrote. An encoding that misses the
// ratio is a *Declined error and nothing is returned to release.
func (l *Layers) Reform(ctx context.Context, body io.Reader, storedSize int64, from *core.StoredForm, want core.StoredFlags) (*Reformed, error) {
	out := &Reformed{Form: &core.StoredForm{ContentHash: from.ContentHash}}
	r, size, err := l.unsealFor(ctx, body, storedSize, from)
	if err != nil {
		return nil, err
	}
	r, size, err = l.recode(r, size, from, want, out)
	if err != nil {
		out.Release()
		return nil, err
	}
	if want.Has(core.StoredEncrypted) {
		if l.Encryptor == nil {
			out.Release()
			return nil, ErrNoEncryptor
		}
		sealed, err := Seal(ctx, l.Encryptor, r, size, nil)
		if err != nil {
			out.Release()
			return nil, err
		}
		r, size = sealed.Body, sealed.Size
		out.Form.Encrypted = true
		out.Form.EncryptionKey = sealed.Form.EncryptionKey
		out.Form.KeyID = sealed.Form.KeyID
		out.Form.PlaintextSize = sealed.Form.PlaintextSize
	}
	out.Body, out.Size = r, size
	return out, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// noop is the closer for paths that opened nothing.
func noop() {
	// Nothing to close.
}

// unsealFor removes the envelope from a copy that has one, so the layers
// underneath can be kept or changed, and reports how many bytes remain.
func (l *Layers) unsealFor(ctx context.Context, body io.Reader, storedSize int64, from *core.StoredForm) (io.Reader, int64, error) {
	r, err := Unseal(ctx, l.Encryptor, body, from)
	if err != nil {
		return nil, 0, err
	}
	if from.Encrypted {
		return r, from.PlaintextSize, nil
	}
	return r, storedSize, nil
}

// recode applies the compression half of a reform: decode a copy that should
// no longer be encoded, encode one that should be, and otherwise carry the
// encoding's description over unchanged. Buffers it opens are added to out's
// release list.
func (l *Layers) recode(r io.Reader, size int64, from *core.StoredForm, want core.StoredFlags, out *Reformed) (io.Reader, int64, error) {
	have := from.Form().Has(core.StoredCompressed)
	switch {
	case have && want.Has(core.StoredCompressed):
		out.Form.CompressionAlgorithm = from.CompressionAlgorithm
		out.Form.CompressionLevel = from.CompressionLevel
		out.Form.CompressionFormatVersion = from.CompressionFormatVersion
		out.Form.LogicalSize = from.LogicalSize
		return r, size, nil
	case have:
		decoded, closeFn, err := Decode(l.Codec, r, from)
		if err != nil {
			return nil, 0, err
		}
		out.release = append(out.release, closeFn)
		return decoded, from.LogicalSize, nil
	case want.Has(core.StoredCompressed):
		return l.encode(r, size, out)
	default:
		return r, size, nil
	}
}

// encode compresses the client's bytes for a reform, declining when the
// encoding does not meet the configured ratio.
func (l *Layers) encode(r io.Reader, logical int64, out *Reformed) (io.Reader, int64, error) {
	if l.Codec == nil {
		return nil, 0, ErrNoCodec
	}
	enc, err := Compress(l.Codec, r, logical, l.Compression.MinRatio)
	if err != nil {
		return nil, 0, err
	}
	if !enc.Worth {
		enc.Body.Cleanup()
		return nil, 0, &Declined{EncodedSize: enc.Size}
	}
	out.release = append(out.release, enc.Body.Cleanup)
	Describe(out.Form, l.Compression.Level, logical)
	return enc.Body.Reader(), enc.Size, nil
}
