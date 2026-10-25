// -------------------------------------------------------------------------------
// Admin CLI - encryption commands (encrypt-existing, decrypt-existing,
// rotate-encryption-key)
//
// Author: Alex Freidah
//
// Bulk encryption maintenance: encrypt-existing walks every plaintext object
// and rewrites it as ciphertext, decrypt-existing reverses that, and
// rotate-encryption-key re-wraps every DEK sealed with -old-key-id under the
// current primary key. The server returns 400 when encryption is not enabled.
// -------------------------------------------------------------------------------

package adminctl

import (
	"flag"
	"fmt"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// cmdEncryptExisting implements `s3-orchestrator admin encrypt-existing
// [-max=N]`. Encrypts every unencrypted object, or the first N of them;
// requires encryption enabled.
func cmdEncryptExisting(args []string, c *client) int {
	return runBulkRewrite(args, c, "encrypt-existing", adminapi.PathEncryptExisting)
}

// cmdDecryptExisting implements `s3-orchestrator admin decrypt-existing
// [-max=N]`. Decrypts every encrypted object back to plaintext, or the first N
// of them; requires encryption enabled for key access.
func cmdDecryptExisting(args []string, c *client) int {
	return runBulkRewrite(args, c, "decrypt-existing", adminapi.PathDecryptExisting)
}

// cmdRotateEncryptionKey implements `s3-orchestrator admin
// rotate-encryption-key -old-key-id=<id>` and `-all-stale`. Re-wraps every DEK
// sealed with the old key, or with -all-stale every DEK not already under the
// current primary key, under the current primary key; the old keys must still
// be configured for unwrapping.
func cmdRotateEncryptionKey(args []string, c *client) int {
	fs := flag.NewFlagSet("rotate-encryption-key", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	oldKeyID := fs.String("old-key-id", "", "Key ID whose DEKs should be re-wrapped")
	allStale := fs.Bool("all-stale", false, "Re-wrap every DEK not already under the current key")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if (*oldKeyID != "") == *allStale {
		fmt.Fprintln(c.stderr, "error: exactly one of -old-key-id or -all-stale is required")
		return 1
	}
	return c.sendJSON(http.MethodPost, adminapi.PathRotateKey,
		adminapi.RotateEncryptionKeyRequest{OldKeyID: *oldKeyID, AllStale: *allStale}, nil)
}
