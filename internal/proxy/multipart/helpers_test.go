// -------------------------------------------------------------------------------
// Multipart Test Helpers
//
// Author: Alex Freidah
//
// Shared setup for the multipart-package tests. The backend double they run
// against is backendtest.InMemory; what is left here is the encryptor the
// encrypted-upload paths need.
// -------------------------------------------------------------------------------

package multipart

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// newTestEncryptor constructs an encryptor with a hard-coded test key for
// multipart-package tests. Mirrors the proxy-package helper.
func newTestEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	p, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewEncryptor(p, 64)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}
