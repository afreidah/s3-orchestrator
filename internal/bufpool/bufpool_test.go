// -------------------------------------------------------------------------------
// Buffer Pool - Unit Tests
//
// Author: Alex Freidah
//
// Verifies correctness of pooled buffer copy operations and Get/Put roundtrip.
// -------------------------------------------------------------------------------

package bufpool

import (
	"bytes"
	"testing"
)

func TestCopy(t *testing.T) {
	data := []byte("hello, buffer pool")
	var dst bytes.Buffer
	n, err := Copy(&dst, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(data)) {
		t.Fatalf("copied %d bytes, want %d", n, len(data))
	}
	if !bytes.Equal(dst.Bytes(), data) {
		t.Fatalf("got %q, want %q", dst.Bytes(), data)
	}
}

func TestGetPutRoundtrip(t *testing.T) {
	b := Get()
	if len(*b) != bufSize {
		t.Fatalf("buffer length %d, want %d", len(*b), bufSize)
	}
	Put(b)

	// After put, Get should return a buffer (possibly the same one)
	b2 := Get()
	if len(*b2) != bufSize {
		t.Fatalf("buffer length %d after roundtrip, want %d", len(*b2), bufSize)
	}
	Put(b2)
}
