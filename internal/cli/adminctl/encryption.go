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
	"encoding/json"
	"flag"
	"fmt"
)

// cmdEncryptExisting implements `s3-orchestrator admin encrypt-existing`.
// Encrypts every unencrypted object in place; requires encryption enabled.
func cmdEncryptExisting(_ []string, c *client) int {
	return c.post("/admin/api/encrypt-existing", "", nil)
}

// cmdDecryptExisting implements `s3-orchestrator admin decrypt-existing`.
// Decrypts every encrypted object back to plaintext; requires encryption
// enabled for key access.
func cmdDecryptExisting(_ []string, c *client) int {
	return c.post("/admin/api/decrypt-existing", "", nil)
}

// cmdRotateEncryptionKey implements `s3-orchestrator admin
// rotate-encryption-key -old-key-id=<id>`. Re-wraps every DEK sealed with the
// old key under the current primary key; the old key must still be present in
// previous_keys for unwrapping.
func cmdRotateEncryptionKey(args []string, c *client) int {
	fs := flag.NewFlagSet("rotate-encryption-key", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	oldKeyID := fs.String("old-key-id", "", "Key ID whose DEKs should be re-wrapped (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *oldKeyID == "" {
		fmt.Fprintln(c.stderr, "error: -old-key-id is required")
		return 1
	}
	body, err := json.Marshal(map[string]string{"old_key_id": *oldKeyID})
	if err != nil {
		fmt.Fprintf(c.stderr, fmtError, err)
		return 1
	}
	return c.post("/admin/api/rotate-encryption-key", string(body), nil)
}
