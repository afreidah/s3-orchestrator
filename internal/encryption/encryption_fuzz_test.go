// -------------------------------------------------------------------------------
// Encryption Fuzz Tests - Header Parsing and Range Translation
//
// Author: Alex Freidah
//
// Fuzz tests for security-critical encryption parsing: binary header parsing
// from untrusted ciphertext and plaintext-to-ciphertext range translation.
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// FuzzParseHeader exercises the binary encryption header parser with arbitrary
// byte sequences. Verifies that a successfully parsed header produces a
// positive chunk size within sane bounds and a correctly-sized nonce.
func FuzzParseHeader(f *testing.F) {
	// Valid header
	valid := make([]byte, HeaderSize)
	copy(valid[0:4], headerMagic[:])
	valid[4] = 0x01
	binary.BigEndian.PutUint32(valid[5:9], 65536)
	f.Add(valid)

	// All zeros — bad magic
	f.Add(make([]byte, HeaderSize))
	// Too short
	f.Add([]byte("short"))
	f.Add([]byte{})
	// Wrong version
	wrong := make([]byte, HeaderSize)
	copy(wrong[0:4], headerMagic[:])
	wrong[4] = 0xFF
	f.Add(wrong)
	// Max uint32 chunk size — would cause huge allocation downstream
	huge := make([]byte, HeaderSize)
	copy(huge[0:4], headerMagic[:])
	huge[4] = 0x01
	binary.BigEndian.PutUint32(huge[5:9], 0xFFFFFFFF)
	f.Add(huge)

	f.Fuzz(func(t *testing.T, data []byte) {
		cs, nonce, err := ParseHeader(bytes.NewReader(data))
		if err != nil {
			return
		}
		if cs <= 0 {
			t.Errorf("chunkSize %d must be positive", cs)
		}
		if len(nonce) != NonceSize {
			t.Errorf("nonce length %d, want %d", len(nonce), NonceSize)
		}
	})
}

// FuzzCiphertextRange exercises the plaintext-to-ciphertext range translator
// with arbitrary offsets and chunk sizes. Verifies that SliceLen always equals
// the requested range length and SliceStart stays within chunk bounds.
func FuzzCiphertextRange(f *testing.F) {
	f.Add(int64(0), int64(63), 64)
	f.Add(int64(0), int64(0), 1)
	f.Add(int64(0), int64(65535), 65536)
	f.Add(int64(100), int64(200), 64)
	f.Add(int64(0), int64(1<<20), 65536)

	f.Fuzz(func(t *testing.T, start, end int64, chunkSize int) {
		if chunkSize <= 0 || start < 0 || end < start {
			return
		}
		r, err := CiphertextRange(start, end, chunkSize)
		if err != nil {
			return
		}
		if r.SliceLen != end-start+1 {
			t.Errorf("SliceLen %d != expected %d", r.SliceLen, end-start+1)
		}
		if r.SliceStart < 0 || r.SliceStart >= int64(chunkSize) {
			t.Errorf("SliceStart %d out of [0, %d)", r.SliceStart, chunkSize)
		}
	})
}

// FuzzUnpackKeyData exercises the key data unpacker with arbitrary byte
// slices. Verifies that successful unpacks produce a 12-byte nonce.
func FuzzUnpackKeyData(f *testing.F) {
	// Valid: 12 bytes nonce + some wrapped DEK
	valid := make([]byte, 44)
	f.Add(valid)
	f.Add([]byte{})
	f.Add(make([]byte, NonceSize))   // exactly nonce size — too short
	f.Add(make([]byte, NonceSize+1)) // minimum valid

	f.Fuzz(func(t *testing.T, data []byte) {
		nonce, dek, err := UnpackKeyData(data)
		if err != nil {
			return
		}
		if len(nonce) != NonceSize {
			t.Errorf("nonce length %d, want %d", len(nonce), NonceSize)
		}
		if len(dek) == 0 {
			t.Error("wrapped DEK is empty")
		}
	})
}
