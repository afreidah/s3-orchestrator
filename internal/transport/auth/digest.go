// -------------------------------------------------------------------------------
// Declared Body Digests
//
// Author: Alex Freidah
//
// A request can state what its body hashes to: Content-MD5, an x-amz-checksum-*
// header, or a hex X-Amz-Content-Sha256 the signature covers. VerifyDigests holds
// the body to each one as it is read, so a mismatch is an error from Read that a
// handler answers like any other failed body.
// -------------------------------------------------------------------------------

package auth

import (
	"bytes"
	"crypto/md5" //nolint:gosec // G501: Content-MD5 is the S3 wire format
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
)

// hexSHA256Len is the length of a hex SHA-256. Anything else in
// X-Amz-Content-Sha256 is a sentinel such as UNSIGNED-PAYLOAD.
const hexSHA256Len = 2 * sha256.Size

// Failures a declared digest reports. ErrPayloadHashMismatch maps to S3
// XAmzContentSHA256Mismatch, ErrDigestMalformed to InvalidDigest for a
// Content-MD5 that is not a base64 MD5, and ErrChecksumMalformed to
// InvalidRequest for a checksum header of the wrong shape.
var (
	ErrPayloadHashMismatch = errors.New("payload hash mismatch")
	ErrDigestMalformed     = errors.New("malformed Content-MD5")
	ErrChecksumMalformed   = errors.New("malformed checksum header")
)

// declaredDigest is one digest a request states for its body, and the error a
// body that disagrees with it reports.
type declaredDigest struct {
	sum      hash.Hash
	want     []byte
	mismatch error
}

// digestReader hashes a body as it is read. It compares on the read that delivers
// the last byte, because a client copying a known length to a backend need not
// read on to EOF. remaining is negative for a body of unknown length, which is
// compared at EOF. A set failed ends the body with that error.
type digestReader struct {
	io.ReadCloser
	digests   []declaredDigest
	remaining int64
	failed    error
}

// VerifyDigests wraps r.Body to check every digest r declares. Call it once the
// body is the client's payload, after any streaming decoder. A malformed digest
// fails the first read, so a request that never reads its body is not refused.
func VerifyDigests(r *http.Request) {
	if r.Body == nil || r.Body == http.NoBody {
		return
	}
	digests, err := declaredDigests(r.Header)
	if err == nil && len(digests) == 0 {
		return
	}
	r.Body = &digestReader{ReadCloser: r.Body, digests: digests, remaining: r.ContentLength, failed: err}
}

// declaredDigests parses every digest h states.
func declaredDigests(h http.Header) ([]declaredDigest, error) {
	var digests []declaredDigest
	if value := h.Get("Content-MD5"); value != "" {
		d, err := base64Digest(md5.New(), value, ErrDigestMalformed) //nolint:gosec // G401: Content-MD5 is the S3 wire format
		if err != nil {
			return nil, err
		}
		digests = append(digests, d)
	}
	for name, newHash := range checksumHashes {
		if value := h.Get(name); value != "" {
			d, err := base64Digest(newHash(), value, ErrChecksumMalformed)
			if err != nil {
				return nil, fmt.Errorf("%w: %s", err, name)
			}
			digests = append(digests, d)
		}
	}
	if value := h.Get("X-Amz-Content-Sha256"); len(value) == hexSHA256Len && isHex(value) {
		want, err := hex.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("decode payload hash: %w", err)
		}
		digests = append(digests, declaredDigest{sum: sha256.New(), want: want, mismatch: ErrPayloadHashMismatch})
	}
	return digests, nil
}

// base64Digest decodes a digest carried in base64, reporting malformed when it is
// not the size sum produces.
func base64Digest(sum hash.Hash, value string, malformed error) (declaredDigest, error) {
	want, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(want) != sum.Size() {
		return declaredDigest{}, malformed
	}
	return declaredDigest{sum: sum, want: want, mismatch: ErrChecksumMismatch}, nil
}

// Read implements io.Reader.
func (d *digestReader) Read(p []byte) (int, error) {
	if d.failed != nil {
		return 0, d.failed
	}
	n, err := d.ReadCloser.Read(p)
	for i := range d.digests {
		_, _ = d.digests[i].sum.Write(p[:n])
	}
	d.remaining -= int64(n)
	if d.remaining == 0 || errors.Is(err, io.EOF) {
		if d.failed = d.compare(); d.failed != nil {
			return n, d.failed
		}
	}
	return n, err
}

// compare returns the error of the first digest the body disagrees with.
func (d *digestReader) compare() error {
	for i := range d.digests {
		if !bytes.Equal(d.digests[i].sum.Sum(nil), d.digests[i].want) {
			return d.digests[i].mismatch
		}
	}
	return nil
}
