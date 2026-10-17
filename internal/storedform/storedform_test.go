// -------------------------------------------------------------------------------
// Stored Form Tests
//
// Author: Alex Freidah
//
// Every stage that encodes a client's bytes for a backend and decodes them
// back: the compression gate and attempt, the envelope seal and check, the
// whole-object open, and the reform a rewrite pass runs between any two stored
// forms. Bytes are round-tripped through a real codec and encryptor so a stage
// that disagrees with another about what a form means shows up as a mismatch.
// -------------------------------------------------------------------------------

package storedform

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// testHash is the content hash seeded forms carry, so a test can see it ride
// through a reform.
const testHash = "client-hash"

// testKeyB64 is the master key every test encryptor wraps under.
const testKeyB64 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// failingProvider is a KeyProvider that cannot wrap a key, which is how a
// seal fails before any byte is written.
type failingProvider struct {
	err error
}

// WrapDEK reports the provider's failure.
// Nothing is wrapped.
func (p *failingProvider) WrapDEK(context.Context, []byte) ([]byte, string, error) {
	return nil, "", p.err
}

// UnwrapDEK reports the provider's failure.
// Nothing is unwrapped.
func (p *failingProvider) UnwrapDEK(context.Context, []byte, string) ([]byte, error) {
	return nil, p.err
}

// KeyID names a key that never wraps anything.
// It only satisfies the interface.
func (*failingProvider) KeyID() string { return "failing" }

// brokenCodec is a Codec whose encode and decode both fail, standing in for
// bytes that will not encode or decode without building them by hand.
type brokenCodec struct {
	err error
}

// Compress reports the codec's failure.
// Nothing is written to dst.
func (c *brokenCodec) Compress(io.Writer, io.Reader) (int64, error) { return 0, c.err }

// DecompressStream reports the codec's failure.
// No reader is opened.
func (c *brokenCodec) DecompressStream(io.Reader) (io.ReadCloser, error) { return nil, c.err }

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// newEncryptor builds an encryptor under the test master key with small
// chunks, so short payloads still span several of them.
func newEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	p, err := encryption.NewConfigKeyProvider(testKeyB64, "test-0")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewEncryptor(p, 64)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// newFailingEncryptor builds an encryptor whose provider refuses to wrap.
// The returned error is the one the provider reports.
func newFailingEncryptor(t *testing.T) (*encryption.Encryptor, error) {
	t.Helper()
	cause := errors.New("simulated KeyProvider failure")
	enc, err := encryption.NewEncryptor(&failingProvider{err: cause}, 64)
	if err != nil {
		t.Fatal(err)
	}
	return enc, cause
}

// newCodec builds a codec at the smallest chunk size and closes it with the
// test.
func newCodec(t *testing.T) *compression.Codec {
	t.Helper()
	c, err := compression.NewCodec(compression.DefaultLevel, compression.MinChunkSize)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// newLayers builds a reform with both layers configured and the default
// compression ratio.
func newLayers(t *testing.T) *Layers {
	t.Helper()
	return &Layers{
		Encryptor:   newEncryptor(t),
		Codec:       newCodec(t),
		Compression: config.CompressionConfig{Enabled: true, Level: "default", MinRatio: 0.95},
	}
}

// compressible is a payload an encoding shrinks well past any ratio.
func compressible() []byte {
	return bytes.Repeat([]byte("stored form round trip payload "), 2000)
}

// incompressible is random bytes no encoding shrinks.
func incompressible(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 64<<10)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// readAll drains r, failing the test on a read error.
func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}

// seed builds the stored bytes and form a write would have produced for plain
// carrying the layers in flags, using the write-side stages directly.
func seed(t *testing.T, l *Layers, plain []byte, flags core.StoredFlags) ([]byte, *core.StoredForm) {
	t.Helper()
	stored, form := plain, &core.StoredForm{ContentHash: testHash}
	if flags.Has(core.StoredCompressed) {
		enc, err := Compress(l.Codec, bytes.NewReader(plain), int64(len(plain)), 1)
		if err != nil {
			t.Fatalf("Compress: %v", err)
		}
		r, err := enc.Body.Reader()
		if err != nil {
			t.Fatalf("encoded reader: %v", err)
		}
		stored = readAll(t, r)
		enc.Body.Cleanup()
		Describe(form, "default", int64(len(plain)))
	}
	if flags.Has(core.StoredEncrypted) {
		sealed, err := Seal(context.Background(), l.Encryptor, bytes.NewReader(stored), int64(len(stored)), nil)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		stored = readAll(t, sealed.Body)
		form.Encrypted = true
		form.EncryptionKey = sealed.Form.EncryptionKey
		form.KeyID = sealed.Form.KeyID
		form.PlaintextSize = sealed.Form.PlaintextSize
	}
	return stored, form
}

// openAll undoes form on stored and returns the client's bytes.
func openAll(t *testing.T, l *Layers, stored []byte, form *core.StoredForm) []byte {
	t.Helper()
	r, closeFn, err := Open(context.Background(), l.Encryptor, l.Codec, bytes.NewReader(stored), form)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer closeFn()
	return readAll(t, r)
}

// flagName renders a flag set for a subtest name.
func flagName(f core.StoredFlags) string {
	switch {
	case f.Has(core.StoredCompressed) && f.Has(core.StoredEncrypted):
		return "both"
	case f.Has(core.StoredCompressed):
		return "compressed"
	case f.Has(core.StoredEncrypted):
		return "encrypted"
	default:
		return "plain"
	}
}

// -------------------------------------------------------------------------
// REFORM
// -------------------------------------------------------------------------

// TestReform_EveryTransition takes a copy from each stored form to each other
// one and checks the result opens back to the client's bytes, describes what
// it holds, reports its own size and keeps the content hash.
func TestReform_EveryTransition(t *testing.T) {
	t.Parallel()
	all := []core.StoredFlags{0, core.StoredCompressed, core.StoredEncrypted, core.StoredLayers}
	plain := compressible()
	for _, from := range all {
		for _, want := range all {
			t.Run(flagName(from)+"_to_"+flagName(want), func(t *testing.T) {
				t.Parallel()
				l := newLayers(t)
				stored, form := seed(t, l, plain, from)

				out, err := l.Reform(context.Background(), bytes.NewReader(stored), int64(len(stored)), form, want)
				if err != nil {
					t.Fatalf("Reform: %v", err)
				}
				got := readAll(t, out.Body)
				out.Release()
				out.Release()
				assertReformed(t, l, plain, got, out.Size, out.Form, want)
				if from.Has(core.StoredCompressed) && want.Has(core.StoredCompressed) &&
					(out.Form.CompressionLevel != form.CompressionLevel || out.Form.LogicalSize != form.LogicalSize) {
					t.Errorf("kept encoding described as %+v, want the source's %+v", out.Form, form)
				}
			})
		}
	}
}

// assertReformed checks reformed bytes match their reported size, carry the
// wanted layers and the content hash, and open back to the client's bytes.
func assertReformed(t *testing.T, l *Layers, plain, got []byte, size int64, form *core.StoredForm, want core.StoredFlags) {
	t.Helper()
	if int64(len(got)) != size {
		t.Errorf("Size = %d, body holds %d", size, len(got))
	}
	if f := form.Form() & core.StoredLayers; f != want {
		t.Errorf("layers = %v, want %v", f, want)
	}
	if form.ContentHash != testHash {
		t.Errorf("ContentHash = %q, want %q", form.ContentHash, testHash)
	}
	if cs := form.ClientSize(size); cs != int64(len(plain)) {
		t.Errorf("ClientSize = %d, want %d", cs, len(plain))
	}
	if !bytes.Equal(openAll(t, l, got, form), plain) {
		t.Error("reformed bytes do not open to the client's bytes")
	}
}

// TestReform_KeptEncodingIsPassedThrough verifies a reform that keeps the
// encoding and adds an envelope seals the stored encoding itself rather than
// decoding and re-encoding it.
func TestReform_KeptEncodingIsPassedThrough(t *testing.T) {
	t.Parallel()
	l := newLayers(t)
	stored, form := seed(t, l, compressible(), core.StoredCompressed)

	out, err := l.Reform(context.Background(), bytes.NewReader(stored), int64(len(stored)), form, core.StoredLayers)
	if err != nil {
		t.Fatalf("Reform: %v", err)
	}
	defer out.Release()
	if len(out.release) != 0 {
		t.Errorf("release list has %d entries, want none for a passed-through encoding", len(out.release))
	}
	if out.Form.PlaintextSize != int64(len(stored)) {
		t.Errorf("PlaintextSize = %d, want the %d encoded bytes", out.Form.PlaintextSize, len(stored))
	}
}

// TestReform_ReleaseFreesBuffers verifies a reform that encodes or decodes
// holds something to release, and that Release empties the list.
func TestReform_ReleaseFreesBuffers(t *testing.T) {
	t.Parallel()
	l := newLayers(t)
	for name, tc := range map[string]struct{ from, want core.StoredFlags }{
		"encode": {from: 0, want: core.StoredCompressed},
		"decode": {from: core.StoredCompressed, want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stored, form := seed(t, l, compressible(), tc.from)
			out, err := l.Reform(context.Background(), bytes.NewReader(stored), int64(len(stored)), form, tc.want)
			if err != nil {
				t.Fatalf("Reform: %v", err)
			}
			_ = readAll(t, out.Body)
			if len(out.release) == 0 {
				t.Fatal("release list is empty, want the buffer the reform opened")
			}
			out.Release()
			if out.release != nil {
				t.Errorf("release list = %d entries after Release, want nil", len(out.release))
			}
		})
	}
}

// TestReform_DeclinedEncoding verifies an encoding that misses the ratio is a
// *Declined carrying the size it came to, with nothing returned to release,
// whether or not the source was sealed.
func TestReform_DeclinedEncoding(t *testing.T) {
	t.Parallel()
	for _, from := range []core.StoredFlags{0, core.StoredEncrypted} {
		t.Run(flagName(from), func(t *testing.T) {
			t.Parallel()
			l := newLayers(t)
			stored, form := seed(t, l, incompressible(t), from)

			out, err := l.Reform(context.Background(), bytes.NewReader(stored), int64(len(stored)), form, form.Form()|core.StoredCompressed)
			var declined *Declined
			if !errors.As(err, &declined) {
				t.Fatalf("error = %v, want *Declined", err)
			}
			if out != nil {
				t.Errorf("Reform returned %+v alongside the decline, want nil", out)
			}
			if declined.EncodedSize <= 0 {
				t.Errorf("EncodedSize = %d, want the encoding's size", declined.EncodedSize)
			}
			if !strings.Contains(declined.Error(), "compression ratio") {
				t.Errorf("Error() = %q, want it to name the ratio", declined.Error())
			}
		})
	}
}

// TestReform_MissingLayer verifies a reform that needs a layer the
// orchestrator has no tool for reports which one, whichever side of the
// reform needs it.
func TestReform_MissingLayer(t *testing.T) {
	t.Parallel()
	full := newLayers(t)
	plainStored, plainForm := seed(t, full, compressible(), 0)
	sealedStored, sealedForm := seed(t, full, compressible(), core.StoredEncrypted)
	codedStored, codedForm := seed(t, full, compressible(), core.StoredCompressed)

	cases := []struct {
		name    string
		layers  *Layers
		stored  []byte
		form    *core.StoredForm
		want    core.StoredFlags
		wantErr error
	}{
		{"seal without encryptor", &Layers{Codec: full.Codec}, plainStored, plainForm, core.StoredEncrypted, ErrNoEncryptor},
		{"encode then seal without encryptor", &Layers{Codec: full.Codec, Compression: full.Compression}, plainStored, plainForm, core.StoredLayers, ErrNoEncryptor},
		{"unseal without encryptor", &Layers{Codec: full.Codec}, sealedStored, sealedForm, 0, ErrNoEncryptor},
		{"encode without codec", &Layers{Encryptor: full.Encryptor}, plainStored, plainForm, core.StoredCompressed, ErrNoCodec},
		{"decode without codec", &Layers{Encryptor: full.Encryptor}, codedStored, codedForm, 0, ErrNoCodec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := tc.layers.Reform(context.Background(), bytes.NewReader(tc.stored), int64(len(tc.stored)), tc.form, tc.want)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if out != nil {
				t.Errorf("Reform returned %+v alongside the error, want nil", out)
			}
		})
	}
}

// TestReform_StageFailures verifies a failure in any stage surfaces and
// returns nothing: an envelope the row does not describe, an encoder that
// fails, a decoder that fails, and a key that cannot be wrapped.
func TestReform_StageFailures(t *testing.T) {
	t.Parallel()
	full := newLayers(t)
	plainStored, plainForm := seed(t, full, compressible(), 0)
	codedStored, codedForm := seed(t, full, compressible(), core.StoredCompressed)
	broken := &brokenCodec{err: errors.New("codec failed")}
	failingEnc, wrapErr := newFailingEncryptor(t)
	mislabeled := &core.StoredForm{Encrypted: true, EncryptionKey: []byte("k"), PlaintextSize: 1}

	cases := []struct {
		name    string
		layers  *Layers
		stored  []byte
		form    *core.StoredForm
		want    core.StoredFlags
		wantErr error
	}{
		{"flag mismatch", full, plainStored, mislabeled, 0, core.ErrEncryptionFlagMismatch},
		{"encode fails", &Layers{Codec: broken}, plainStored, plainForm, core.StoredCompressed, broken.err},
		{"decode fails", &Layers{Codec: broken}, codedStored, codedForm, 0, broken.err},
		{"seal fails", &Layers{Encryptor: failingEnc, Codec: full.Codec, Compression: full.Compression}, plainStored, plainForm, core.StoredLayers, wrapErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := tc.layers.Reform(context.Background(), bytes.NewReader(tc.stored), int64(len(tc.stored)), tc.form, tc.want)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if out != nil {
				t.Errorf("Reform returned %+v alongside the error, want nil", out)
			}
		})
	}
}

// -------------------------------------------------------------------------
// ENVELOPE
// -------------------------------------------------------------------------

// TestCheckEnvelope verifies a row and its bytes that agree pass with every
// byte still readable, and that a disagreement in either direction is a flag
// mismatch.
func TestCheckEnvelope(t *testing.T) {
	t.Parallel()
	l := newLayers(t)
	sealed, sealedForm := seed(t, l, []byte("envelope payload"), core.StoredEncrypted)
	plain := []byte("plaintext payload")

	cases := []struct {
		name     string
		stored   []byte
		form     *core.StoredForm
		mismatch bool
	}{
		{"plain matches", plain, &core.StoredForm{}, false},
		{"sealed matches", sealed, sealedForm, false},
		{"row says sealed, bytes plain", plain, sealedForm, true},
		{"row says plain, bytes sealed", sealed, &core.StoredForm{}, true},
		{"short object is plain", []byte("ab"), &core.StoredForm{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, err := CheckEnvelope(bytes.NewReader(tc.stored), tc.form)
			if tc.mismatch != errors.Is(err, core.ErrEncryptionFlagMismatch) {
				t.Fatalf("error = %v, want mismatch=%t", err, tc.mismatch)
			}
			if !tc.mismatch && err != nil {
				t.Fatalf("CheckEnvelope: %v", err)
			}
			if body == nil {
				t.Fatal("body = nil, want the bytes replayed")
			}
			if got := readAll(t, body); !bytes.Equal(got, tc.stored) {
				t.Errorf("replayed %d bytes, want the %d stored", len(got), len(tc.stored))
			}
		})
	}
}

// TestCheckEnvelope_ReadError verifies a body that cannot be read surfaces
// the read error and no reader.
func TestCheckEnvelope_ReadError(t *testing.T) {
	t.Parallel()
	cause := errors.New("connection reset")
	body, err := CheckEnvelope(iotest.ErrReader(cause), &core.StoredForm{})
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want the read error", err)
	}
	if body != nil {
		t.Error("body returned alongside a read error")
	}
}

// TestUnseal verifies plaintext passes through, an envelope opens to what was
// sealed, and an envelope with no encryptor or a bad key fails.
func TestUnseal(t *testing.T) {
	t.Parallel()
	l := newLayers(t)
	payload := []byte("unseal payload spanning more than one chunk of sixty-four bytes")
	sealed, form := seed(t, l, payload, core.StoredEncrypted)
	ctx := context.Background()

	r, err := Unseal(ctx, l.Encryptor, bytes.NewReader(payload), &core.StoredForm{})
	if err != nil || !bytes.Equal(readAll(t, r), payload) {
		t.Errorf("plaintext Unseal = %v, want the bytes unchanged", err)
	}
	r, err = Unseal(ctx, l.Encryptor, bytes.NewReader(sealed), form)
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	if !bytes.Equal(readAll(t, r), payload) {
		t.Error("unsealed bytes differ from what was sealed")
	}
	if _, err := Unseal(ctx, nil, bytes.NewReader(sealed), form); !errors.Is(err, ErrNoEncryptor) {
		t.Errorf("no encryptor: error = %v, want ErrNoEncryptor", err)
	}
	bad := *form
	bad.EncryptionKey = []byte("tiny")
	if _, err := Unseal(ctx, l.Encryptor, bytes.NewReader(sealed), &bad); !errors.Is(err, encryption.ErrInvalidKeyData) {
		t.Errorf("bad key: error = %v, want ErrInvalidKeyData", err)
	}
}

// TestDecode verifies an uncompressed form passes through with a closer that
// is safe to call, and a compressed one decodes or reports why it could not.
func TestDecode(t *testing.T) {
	t.Parallel()
	l := newLayers(t)
	payload := compressible()
	coded, form := seed(t, l, payload, core.StoredCompressed)

	r, closeFn, err := Decode(nil, bytes.NewReader(payload), &core.StoredForm{})
	if err != nil || !bytes.Equal(readAll(t, r), payload) {
		t.Errorf("uncompressed Decode = %v, want the bytes unchanged", err)
	}
	closeFn()

	r, closeFn, err = Decode(l.Codec, bytes.NewReader(coded), form)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(readAll(t, r), payload) {
		t.Error("decoded bytes differ from the client's")
	}
	closeFn()

	if _, closeFn, err = Decode(nil, bytes.NewReader(coded), form); !errors.Is(err, ErrNoCodec) {
		t.Errorf("no codec: error = %v, want ErrNoCodec", err)
	}
	closeFn()
}

// TestDecode_FailureCounted verifies a decoder failure surfaces and counts a
// decode error.
func TestDecode_FailureCounted(t *testing.T) {
	counter := telemetry.CompressionErrorsTotal.WithLabelValues(telemetry.CompressionOpDecode)
	before := testutil.ToFloat64(counter)
	broken := &brokenCodec{err: errors.New("bad frame")}
	form := Describe(nil, "default", 10)

	_, closeFn, err := Decode(broken, strings.NewReader("x"), form)
	closeFn()
	if !errors.Is(err, broken.err) {
		t.Fatalf("error = %v, want the decoder's", err)
	}
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("decode errors moved by %v, want 1", got)
	}
}

// TestOpen_StopsAtTheEnvelope verifies Open reports an envelope it cannot
// open without reaching the decoder.
func TestOpen_StopsAtTheEnvelope(t *testing.T) {
	t.Parallel()
	l := newLayers(t)
	stored, form := seed(t, l, compressible(), core.StoredLayers)

	_, closeFn, err := Open(context.Background(), nil, &brokenCodec{err: errors.New("must not decode")}, bytes.NewReader(stored), form)
	closeFn()
	if !errors.Is(err, ErrNoEncryptor) {
		t.Errorf("error = %v, want ErrNoEncryptor", err)
	}
}

// -------------------------------------------------------------------------
// COMPRESSION
// -------------------------------------------------------------------------

// TestShouldCompress verifies the gate: no codec or a disabled config never
// compresses, a write under the minimum is declined and counted, and one at
// the minimum is compressed.
func TestShouldCompress(t *testing.T) {
	codec := newCodec(t)
	enabled := &config.CompressionConfig{Enabled: true, MinSize: 100}
	skipped := telemetry.CompressionSkippedTotal.WithLabelValues(telemetry.CompressionSkipMinSize)

	if ShouldCompress(enabled, nil, 1000) {
		t.Error("compressed with no codec")
	}
	if ShouldCompress(&config.CompressionConfig{MinSize: 100}, codec, 1000) {
		t.Error("compressed with compression disabled")
	}
	before := testutil.ToFloat64(skipped)
	if ShouldCompress(enabled, codec, 99) {
		t.Error("compressed a write under the minimum")
	}
	if got := testutil.ToFloat64(skipped) - before; got != 1 {
		t.Errorf("min-size skips moved by %v, want 1", got)
	}
	if !ShouldCompress(enabled, codec, 100) {
		t.Error("declined a write at the minimum")
	}
}

// TestCompress_Worth verifies an encoding that meets the ratio is handed back
// worth storing, with its size matching the bytes buffered, and decodes to the
// input.
func TestCompress_Worth(t *testing.T) {
	t.Parallel()
	codec := newCodec(t)
	payload := compressible()

	enc, err := Compress(codec, bytes.NewReader(payload), int64(len(payload)), 0.95)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	defer enc.Body.Cleanup()
	if !enc.Worth {
		t.Fatal("Worth = false for a payload that compresses well")
	}
	if enc.Size >= int64(len(payload)) || enc.Body.Size() != enc.Size {
		t.Errorf("Size = %d, body = %d, payload = %d", enc.Size, enc.Body.Size(), len(payload))
	}
	r, err := enc.Body.Reader()
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	decoded, closeFn, err := Decode(codec, r, Describe(nil, "default", int64(len(payload))))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	defer closeFn()
	if !bytes.Equal(readAll(t, decoded), payload) {
		t.Error("encoding does not decode to the input")
	}
}

// TestCompress_NotWorth verifies an encoding that misses the ratio is handed
// back with Worth false and counted as a min-ratio skip.
func TestCompress_NotWorth(t *testing.T) {
	codec := newCodec(t)
	payload := incompressible(t)
	skipped := telemetry.CompressionSkippedTotal.WithLabelValues(telemetry.CompressionSkipMinRatio)
	before := testutil.ToFloat64(skipped)

	enc, err := Compress(codec, bytes.NewReader(payload), int64(len(payload)), 0.95)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	defer enc.Body.Cleanup()
	if enc.Worth {
		t.Error("Worth = true for random bytes")
	}
	if got := testutil.ToFloat64(skipped) - before; got != 1 {
		t.Errorf("min-ratio skips moved by %v, want 1", got)
	}
}

// TestCompress_EncoderFailure verifies an encoder failure surfaces and counts
// an encode error.
func TestCompress_EncoderFailure(t *testing.T) {
	counter := telemetry.CompressionErrorsTotal.WithLabelValues(telemetry.CompressionOpEncode)
	before := testutil.ToFloat64(counter)
	broken := &brokenCodec{err: errors.New("encoder failed")}

	enc, err := Compress(broken, strings.NewReader("payload"), 7, 0.95)
	if !errors.Is(err, broken.err) || enc != nil {
		t.Fatalf("Compress = %+v, %v, want nil and the encoder's error", enc, err)
	}
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("encode errors moved by %v, want 1", got)
	}
}

// TestDescribe verifies a nil form is allocated and an existing one is filled
// in place, keeping the fields it already carried.
func TestDescribe(t *testing.T) {
	t.Parallel()
	got := Describe(nil, "best", 500)
	if got == nil || got.CompressionAlgorithm != compression.Algorithm || got.CompressionLevel != "best" ||
		got.CompressionFormatVersion != compression.FormatVersion || got.LogicalSize != 500 {
		t.Errorf("Describe(nil) = %+v", got)
	}

	existing := &core.StoredForm{Encrypted: true, KeyID: "kid", ContentHash: "h"}
	if Describe(existing, "default", 42) != existing {
		t.Fatal("Describe allocated a new form for an existing one")
	}
	if existing.LogicalSize != 42 || existing.KeyID != "kid" || existing.ContentHash != "h" || !existing.Encrypted {
		t.Errorf("existing form = %+v, want the encoding added and the rest kept", existing)
	}
}

// -------------------------------------------------------------------------
// SEAL
// -------------------------------------------------------------------------

// TestSeal_MintsAKey verifies a seal with no key describes the envelope it
// produced and opens back to the input under that description.
func TestSeal_MintsAKey(t *testing.T) {
	t.Parallel()
	enc := newEncryptor(t)
	payload := []byte("sealed under a freshly minted data key")

	sealed, err := Seal(context.Background(), enc, bytes.NewReader(payload), int64(len(payload)), nil)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ciphertext := readAll(t, sealed.Body)
	if int64(len(ciphertext)) != sealed.Size || sealed.Size != enc.CiphertextSize(int64(len(payload))) {
		t.Errorf("Size = %d, body = %d", sealed.Size, len(ciphertext))
	}
	f := sealed.Form
	if !f.Encrypted || f.KeyID != "test-0" || f.PlaintextSize != int64(len(payload)) || f.ContentHash != "" {
		t.Errorf("form = %+v", f)
	}
	r, err := Unseal(context.Background(), enc, bytes.NewReader(ciphertext), f)
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	if !bytes.Equal(readAll(t, r), payload) {
		t.Error("sealed bytes do not open to the input")
	}
}

// TestSeal_UnderAGivenKey verifies a seal under a caller's data key records
// that key rather than minting one.
func TestSeal_UnderAGivenKey(t *testing.T) {
	t.Parallel()
	enc := newEncryptor(t)
	dek, wrapped, keyID, err := enc.GenerateAndWrapDEK(context.Background())
	if err != nil {
		t.Fatalf("GenerateAndWrapDEK: %v", err)
	}
	payload := []byte("sealed under the upload's key")

	sealed, err := Seal(context.Background(), enc, bytes.NewReader(payload), int64(len(payload)), &DataKey{DEK: dek, WrappedDEK: wrapped, KeyID: keyID})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	_, gotWrapped, err := encryption.UnpackKeyData(sealed.Form.EncryptionKey)
	if err != nil {
		t.Fatalf("UnpackKeyData: %v", err)
	}
	if !bytes.Equal(gotWrapped, wrapped) || sealed.Form.KeyID != keyID {
		t.Error("form does not record the caller's data key")
	}
	r, err := Unseal(context.Background(), enc, sealed.Body, sealed.Form)
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	if !bytes.Equal(readAll(t, r), payload) {
		t.Error("sealed bytes do not open to the input")
	}
}

// TestSeal_WrapFailure verifies a provider that cannot wrap fails the seal
// with its error in the chain and counts an encrypt failure.
func TestSeal_WrapFailure(t *testing.T) {
	enc, cause := newFailingEncryptor(t)
	counter := telemetry.EncryptionErrorsTotal.WithLabelValues(encryptOp, "encrypt_failed")
	before := testutil.ToFloat64(counter)

	sealed, err := Seal(context.Background(), enc, strings.NewReader("doomed"), 6, nil)
	if !errors.Is(err, cause) || sealed != nil {
		t.Fatalf("Seal = %+v, %v, want nil and the provider's error", sealed, err)
	}
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("encrypt failures moved by %v, want 1", got)
	}
}

// -------------------------------------------------------------------------
// STREAM METRIC
// -------------------------------------------------------------------------

// TestWithStreamMetric_CountsNonEOFError verifies a read error mid-stream
// counts a stream failure under the wrapper's op.
func TestWithStreamMetric_CountsNonEOFError(t *testing.T) {
	counter := telemetry.EncryptionErrorsTotal.WithLabelValues(encryptOp, "stream_failed")
	before := testutil.ToFloat64(counter)

	wrapped := WithStreamMetric(iotest.ErrReader(errors.New("synthetic transport failure")), encryptOp)
	if _, err := wrapped.Read(make([]byte, 16)); err == nil {
		t.Fatal("expected error from wrapped reader")
	}
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("stream failures moved by %v, want 1", got)
	}
}

// TestWithStreamMetric_IgnoresEOF verifies a clean end of stream counts
// nothing.
func TestWithStreamMetric_IgnoresEOF(t *testing.T) {
	counter := telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "stream_failed")
	before := testutil.ToFloat64(counter)

	if got := readAll(t, WithStreamMetric(strings.NewReader("hello"), "decrypt")); string(got) != "hello" {
		t.Errorf("read %q, want hello", got)
	}
	if got := testutil.ToFloat64(counter) - before; got != 0 {
		t.Errorf("stream failures moved by %v, want 0 on clean EOF", got)
	}
}
