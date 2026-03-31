// -------------------------------------------------------------------------------
// Integrity Verification Tests
//
// Author: Alex Freidah
//
// Tests for content hashing and streaming verification.
// -------------------------------------------------------------------------------

package proxy

import (
	"io"
	"strings"
	"testing"
)

func TestHashBody(t *testing.T) {
	t.Parallel()
	hash := HashBody([]byte("hello world"))
	expected := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if hash != expected {
		t.Errorf("HashBody = %s, want %s", hash, expected)
	}
}

func TestHashBody_Empty(t *testing.T) {
	t.Parallel()
	hash := HashBody(nil)
	expected := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if hash != expected {
		t.Errorf("HashBody(nil) = %s, want %s", hash, expected)
	}
}

func TestVerifyingReader_Match(t *testing.T) {
	t.Parallel()
	data := "test data for hashing"
	expected := HashBody([]byte(data))

	r := io.NopCloser(strings.NewReader(data))
	vr := NewVerifyingReader(r)
	_, _ = io.ReadAll(vr)

	if err := vr.Verify(expected); err != nil {
		t.Errorf("Verify should pass: %v", err)
	}
}

func TestVerifyingReader_Mismatch(t *testing.T) {
	t.Parallel()
	r := io.NopCloser(strings.NewReader("actual data"))
	vr := NewVerifyingReader(r)
	_, _ = io.ReadAll(vr)

	if err := vr.Verify("0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Error("Verify should fail on hash mismatch")
	}
}

func TestVerifyingReader_EmptyExpected(t *testing.T) {
	t.Parallel()
	r := io.NopCloser(strings.NewReader("any data"))
	vr := NewVerifyingReader(r)
	_, _ = io.ReadAll(vr)

	if err := vr.Verify(""); err != nil {
		t.Errorf("Verify with empty expected should pass: %v", err)
	}
}

func TestVerifyingReader_OnMismatchCallback(t *testing.T) {
	t.Parallel()
	r := io.NopCloser(strings.NewReader("actual data"))
	vr := NewVerifyingReader(r)

	called := false
	vr.SetVerification("0000000000000000000000000000000000000000000000000000000000000000", func(expected, actual string) {
		called = true
	})

	_, _ = io.ReadAll(vr)
	_ = vr.Close()

	if !called {
		t.Error("onMismatch callback should have been called")
	}
}

func TestVerifyingReader_OnMatchNoCallback(t *testing.T) {
	t.Parallel()
	data := "matching data"
	expected := HashBody([]byte(data))

	r := io.NopCloser(strings.NewReader(data))
	vr := NewVerifyingReader(r)

	called := false
	vr.SetVerification(expected, func(expected, actual string) {
		called = true
	})

	_, _ = io.ReadAll(vr)
	_ = vr.Close()

	if called {
		t.Error("onMismatch callback should not be called on matching hash")
	}
}
