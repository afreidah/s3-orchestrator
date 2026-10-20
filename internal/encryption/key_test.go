// -------------------------------------------------------------------------------
// Key Provider Tests
//
// Author: Alex Freidah
//
// Tests for StaticKeyProvider, key fingerprints, and MultiKeyProvider including
// DEK wrap/unwrap round-trips, key rotation with multiple providers, IDs from
// earlier releases, and error cases for invalid keys.
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// randomKey returns a fresh random master key of the configured size.
// Each call gives a distinct key and so a distinct fingerprint.
func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, config.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return key
}

// staticProvider builds a StaticKeyProvider for key and fails the test on
// error, for callers that only pass valid keys.
func staticProvider(t *testing.T, key []byte) *StaticKeyProvider {
	t.Helper()
	p, err := NewStaticKeyProvider(key)
	if err != nil {
		t.Fatalf("NewStaticKeyProvider: %v", err)
	}
	return p
}

// writeKeyFile writes key to a file in a per-test directory and returns its
// path, the form master_key_file and previous_key_files read.
func writeKeyFile(t *testing.T, key []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// wrapDEK wraps a fresh random DEK with p and returns the DEK and its wrapped
// form, the inputs every unwrap test starts from.
func wrapDEK(t *testing.T, p KeyProvider) (dek, wrapped []byte) {
	t.Helper()
	dek = randomKey(t)
	wrapped, _, err := p.WrapDEK(context.Background(), dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	return dek, wrapped
}

// assertUnwraps checks that p unwraps wrapped, recorded under keyID, back to
// dek.
func assertUnwraps(t *testing.T, p KeyProvider, wrapped []byte, keyID string, dek []byte) {
	t.Helper()
	got, err := p.UnwrapDEK(context.Background(), wrapped, keyID)
	if err != nil {
		t.Fatalf("UnwrapDEK(%q): %v", keyID, err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatalf("UnwrapDEK(%q) returned a different DEK", keyID)
	}
}

// -------------------------------------------------------------------------
// FINGERPRINT
// -------------------------------------------------------------------------

// TestFingerprint_StableAndDistinct asserts a key always fingerprints to the
// same ID, pinned for the all-zero key so the format cannot drift between
// releases, and that two keys get different IDs.
func TestFingerprint_StableAndDistinct(t *testing.T) {
	t.Parallel()
	zero := make([]byte, config.KeySize)
	if got := Fingerprint(zero); got != "k-12ffa798e8e831c3" {
		t.Errorf("Fingerprint(zero key) = %q, want %q", got, "k-12ffa798e8e831c3")
	}

	key := randomKey(t)
	if Fingerprint(key) != Fingerprint(bytes.Clone(key)) {
		t.Error("Fingerprint differs for the same key")
	}
	if Fingerprint(key) == Fingerprint(randomKey(t)) {
		t.Error("Fingerprint is the same for two different keys")
	}
}

// -------------------------------------------------------------------------
// STATIC KEY PROVIDER
// -------------------------------------------------------------------------

// TestStaticKeyProvider_WrapUnwrap asserts the provider records DEKs under
// the key fingerprint and unwraps what it wrapped.
func TestStaticKeyProvider_WrapUnwrap(t *testing.T) {
	t.Parallel()
	key := randomKey(t)
	p := staticProvider(t, key)

	if p.KeyID() != Fingerprint(key) {
		t.Errorf("KeyID = %q, want %q", p.KeyID(), Fingerprint(key))
	}

	dek := randomKey(t)
	wrapped, keyID, err := p.WrapDEK(context.Background(), dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if keyID != Fingerprint(key) {
		t.Errorf("WrapDEK keyID = %q, want %q", keyID, Fingerprint(key))
	}
	assertUnwraps(t, p, wrapped, keyID, dek)
}

// TestStaticKeyProvider_RejectsOtherKeysDEK asserts a DEK another key wrapped
// fails authentication, which is what lets MultiKeyProvider try keys in turn.
func TestStaticKeyProvider_RejectsOtherKeysDEK(t *testing.T) {
	t.Parallel()
	_, wrapped := wrapDEK(t, staticProvider(t, randomKey(t)))

	if _, err := staticProvider(t, randomKey(t)).UnwrapDEK(context.Background(), wrapped, ""); err == nil {
		t.Error("UnwrapDEK under a different key succeeded")
	}
}

// TestNewStaticKeyProvider_RejectsWrongSize asserts every key length other
// than 32 bytes is refused.
func TestNewStaticKeyProvider_RejectsWrongSize(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := NewStaticKeyProvider(make([]byte, n)); err == nil {
			t.Errorf("NewStaticKeyProvider(%d bytes) succeeded", n)
		}
	}
}

// TestNewGCM_RejectsInvalidKey asserts the shared cipher constructor reports
// a key length AES does not accept.
func TestNewGCM_RejectsInvalidKey(t *testing.T) {
	t.Parallel()
	if _, err := newGCM(make([]byte, 7)); err == nil {
		t.Error("newGCM(7 bytes) succeeded")
	}
}

// -------------------------------------------------------------------------
// MULTI-KEY PROVIDER (KEY ROTATION)
// -------------------------------------------------------------------------

// rotationPair returns a MultiKeyProvider over a fresh primary and one fresh
// previous key, with both underlying providers.
func rotationPair(t *testing.T) (multi *MultiKeyProvider, primary, previous *StaticKeyProvider) {
	t.Helper()
	primary = staticProvider(t, randomKey(t))
	previous = staticProvider(t, randomKey(t))
	return NewMultiKeyProvider(primary, []KeyProvider{previous}), primary, previous
}

// TestMultiKeyProvider_WrapUsePrimary asserts new DEKs are wrapped with the
// primary key and recorded under its ID.
func TestMultiKeyProvider_WrapUsePrimary(t *testing.T) {
	t.Parallel()
	multi, primary, _ := rotationPair(t)

	if multi.KeyID() != primary.KeyID() {
		t.Errorf("KeyID = %q, want %q", multi.KeyID(), primary.KeyID())
	}
	_, keyID, err := multi.WrapDEK(context.Background(), randomKey(t))
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if keyID != primary.KeyID() {
		t.Errorf("WrapDEK used %q, want %q", keyID, primary.KeyID())
	}
}

// TestMultiKeyProvider_UnwrapByRecordedID asserts a DEK recorded under the
// primary or a previous key fingerprint unwraps with that key.
func TestMultiKeyProvider_UnwrapByRecordedID(t *testing.T) {
	t.Parallel()
	multi, primary, previous := rotationPair(t)

	for _, p := range []*StaticKeyProvider{primary, previous} {
		dek, wrapped := wrapDEK(t, p)
		assertUnwraps(t, multi, wrapped, p.KeyID(), dek)
	}
}

// TestMultiKeyProvider_UnwrapsLegacyPositionalID asserts a DEK recorded under
// an ID no key carries, as earlier releases recorded "config-0" and "file-0",
// still unwraps with whichever configured key wrapped it.
func TestMultiKeyProvider_UnwrapsLegacyPositionalID(t *testing.T) {
	t.Parallel()
	multi, primary, previous := rotationPair(t)

	for _, tc := range []struct {
		name    string
		wrapper KeyProvider
		keyID   string
	}{
		{"previous key", previous, "config-0"},
		{"primary key", primary, "file-0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dek, wrapped := wrapDEK(t, tc.wrapper)
			assertUnwraps(t, multi, wrapped, tc.keyID, dek)
		})
	}
}

// TestMultiKeyProvider_UnknownKeyIDReturnsError asserts a DEK no configured
// key unwraps fails and is counted as an unknown key ID.
func TestMultiKeyProvider_UnknownKeyIDReturnsError(t *testing.T) {
	t.Parallel()
	multi, _, _ := rotationPair(t)
	_, wrapped := wrapDEK(t, staticProvider(t, randomKey(t)))

	before := promtest.ToFloat64(telemetry.EncryptionUnknownKeyIDTotal)
	_, err := multi.UnwrapDEK(context.Background(), wrapped, "config-0")
	if err == nil {
		t.Fatal("UnwrapDEK of a DEK no configured key wrapped succeeded")
	}
	if !strings.Contains(err.Error(), `no configured key unwraps key ID "config-0"`) {
		t.Errorf("error should name the key ID, got: %v", err)
	}
	if after := promtest.ToFloat64(telemetry.EncryptionUnknownKeyIDTotal); after != before+1 {
		t.Errorf("EncryptionUnknownKeyIDTotal = %v, want %v", after, before+1)
	}
}

// -------------------------------------------------------------------------
// NEW KEY PROVIDER FROM CONFIG
// -------------------------------------------------------------------------

// TestNewKeyProviderFromConfig_InlineAndFileShareID asserts the same key gives
// the same ID whether it is configured inline or as a file.
func TestNewKeyProviderFromConfig_InlineAndFileShareID(t *testing.T) {
	t.Parallel()
	key := randomKey(t)

	for _, cfg := range []*config.EncryptionConfig{
		{MasterKey: base64.StdEncoding.EncodeToString(key)},
		{MasterKeyFile: writeKeyFile(t, key)},
	} {
		p, err := NewKeyProviderFromConfig(cfg)
		if err != nil {
			t.Fatalf("NewKeyProviderFromConfig: %v", err)
		}
		if _, ok := p.(*StaticKeyProvider); !ok {
			t.Errorf("provider = %T, want *StaticKeyProvider without previous keys", p)
		}
		if p.KeyID() != Fingerprint(key) {
			t.Errorf("KeyID = %q, want %q", p.KeyID(), Fingerprint(key))
		}
	}
}

// TestNewKeyProviderFromConfig_Vault verifies the new key provider from config vault contract.
// Asserts that NewKeyProviderFromConfig:.
func TestNewKeyProviderFromConfig_Vault(t *testing.T) {
	t.Parallel()
	cfg := &config.EncryptionConfig{
		Vault: &config.VaultTransitConfig{
			Address:   "https://vault.example.com:8200",
			Token:     "test-token",
			KeyName:   "my-key",
			MountPath: "transit",
		},
	}
	p, err := NewKeyProviderFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewKeyProviderFromConfig: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := p.(interface{ Close() }); ok {
			closer.Close()
		}
	})
	if p.KeyID() != "vault:transit/my-key" {
		t.Errorf("KeyID = %q, want %q", p.KeyID(), "vault:transit/my-key")
	}
}

// TestNewKeyProviderFromConfig_NoKeySource verifies the new key provider from config no key source behaviour described by the test name.
func TestNewKeyProviderFromConfig_NoKeySource(t *testing.T) {
	t.Parallel()
	cfg := &config.EncryptionConfig{}
	_, err := NewKeyProviderFromConfig(cfg)
	if err == nil {
		t.Error("expected error when no key source configured")
	}
}

// TestNewKeyProviderFromConfig_WithPreviousKeys asserts previous keys from
// both previous_keys and previous_key_files unwrap DEKs recorded under their
// fingerprints and under the positional IDs earlier releases recorded, while
// new DEKs go to the primary.
func TestNewKeyProviderFromConfig_WithPreviousKeys(t *testing.T) {
	t.Parallel()
	primaryKey, inlineKey, fileKey := randomKey(t), randomKey(t), randomKey(t)

	p, err := NewKeyProviderFromConfig(&config.EncryptionConfig{
		MasterKey:        base64.StdEncoding.EncodeToString(primaryKey),
		PreviousKeys:     []string{base64.StdEncoding.EncodeToString(inlineKey)},
		PreviousKeyFiles: []string{writeKeyFile(t, fileKey)},
	})
	if err != nil {
		t.Fatalf("NewKeyProviderFromConfig: %v", err)
	}
	if _, ok := p.(*MultiKeyProvider); !ok {
		t.Fatalf("provider = %T, want *MultiKeyProvider", p)
	}
	if p.KeyID() != Fingerprint(primaryKey) {
		t.Errorf("KeyID = %q, want %q", p.KeyID(), Fingerprint(primaryKey))
	}

	for _, key := range [][]byte{primaryKey, inlineKey, fileKey} {
		dek, wrapped := wrapDEK(t, staticProvider(t, key))
		assertUnwraps(t, p, wrapped, Fingerprint(key), dek)
		assertUnwraps(t, p, wrapped, "config-1", dek)
	}
}

// TestNewKeyProviderFromConfig_PreviousKeyFilesOnly asserts a key file alone
// is enough to build the rotation provider.
func TestNewKeyProviderFromConfig_PreviousKeyFilesOnly(t *testing.T) {
	t.Parallel()
	fileKey := randomKey(t)

	p, err := NewKeyProviderFromConfig(&config.EncryptionConfig{
		MasterKey:        base64.StdEncoding.EncodeToString(randomKey(t)),
		PreviousKeyFiles: []string{writeKeyFile(t, fileKey)},
	})
	if err != nil {
		t.Fatalf("NewKeyProviderFromConfig: %v", err)
	}
	dek, wrapped := wrapDEK(t, staticProvider(t, fileKey))
	assertUnwraps(t, p, wrapped, Fingerprint(fileKey), dek)
}

// TestNewKeyProviderFromConfig_InvalidKeys asserts every unusable key source,
// primary or previous, fails the build and names the offending field.
func TestNewKeyProviderFromConfig_InvalidKeys(t *testing.T) {
	t.Parallel()
	valid := base64.StdEncoding.EncodeToString(randomKey(t))
	missing := filepath.Join(t.TempDir(), "absent.key")

	for _, tc := range []struct {
		name    string
		cfg     config.EncryptionConfig
		wantErr string
	}{
		{"primary base64", config.EncryptionConfig{MasterKey: "not-valid-base64!!!"}, ""},
		{"primary file", config.EncryptionConfig{MasterKeyFile: missing}, ""},
		{"previous key", config.EncryptionConfig{MasterKey: valid, PreviousKeys: []string{"not-valid-base64!!!"}}, "previous_keys[0]"},
		{"previous key file", config.EncryptionConfig{MasterKey: valid, PreviousKeyFiles: []string{missing}}, "previous_key_files[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewKeyProviderFromConfig(&tc.cfg)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}
