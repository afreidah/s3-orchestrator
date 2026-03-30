// -------------------------------------------------------------------------------
// Key Provider Tests
//
// Author: Alex Freidah
//
// Tests for ConfigKeyProvider, FileKeyProvider, and MultiKeyProvider including
// DEK wrap/unwrap round-trips, key rotation with multiple providers, and
// error cases for invalid keys.
// -------------------------------------------------------------------------------

package encryption

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
)

// -------------------------------------------------------------------------
// CONFIG KEY PROVIDER
// -------------------------------------------------------------------------

func TestConfigKeyProvider_WrapUnwrap(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	b64 := base64.StdEncoding.EncodeToString(key)

	p, err := NewConfigKeyProvider(b64, "test-key")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}

	if p.KeyID() != "test-key" {
		t.Errorf("KeyID = %q, want %q", p.KeyID(), "test-key")
	}

	ctx := context.Background()
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)

	wrapped, keyID, err := p.WrapDEK(ctx, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if keyID != "test-key" {
		t.Errorf("WrapDEK keyID = %q, want %q", keyID, "test-key")
	}

	unwrapped, err := p.UnwrapDEK(ctx, wrapped, keyID)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}

	if len(unwrapped) != len(dek) {
		t.Fatalf("unwrapped len = %d, want %d", len(unwrapped), len(dek))
	}
	for i := range dek {
		if unwrapped[i] != dek[i] {
			t.Fatalf("unwrapped[%d] = %d, want %d", i, unwrapped[i], dek[i])
		}
	}
}

func TestConfigKeyProvider_DefaultKeyID(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	b64 := base64.StdEncoding.EncodeToString(key)

	p, err := NewConfigKeyProvider(b64, "")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}

	if p.KeyID() != "config-0" {
		t.Errorf("KeyID = %q, want %q", p.KeyID(), "config-0")
	}
}

func TestConfigKeyProvider_InvalidBase64(t *testing.T) {
	t.Parallel()
	_, err := NewConfigKeyProvider("not-valid-base64!!!", "test")
	if err == nil {
		t.Error("expected error for invalid base64")
	}
}

func TestConfigKeyProvider_WrongKeyLength(t *testing.T) {
	t.Parallel()
	key := make([]byte, 16) // too short
	b64 := base64.StdEncoding.EncodeToString(key)

	_, err := NewConfigKeyProvider(b64, "test")
	if err == nil {
		t.Error("expected error for 16-byte key")
	}
}

// -------------------------------------------------------------------------
// FILE KEY PROVIDER
// -------------------------------------------------------------------------

func TestFileKeyProvider_WrapUnwrap(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	path := filepath.Join(t.TempDir(), "test.key")
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p, err := NewFileKeyProvider(path, "file-test")
	if err != nil {
		t.Fatalf("NewFileKeyProvider: %v", err)
	}

	if p.KeyID() != "file-test" {
		t.Errorf("KeyID = %q, want %q", p.KeyID(), "file-test")
	}

	ctx := context.Background()
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)

	wrapped, keyID, err := p.WrapDEK(ctx, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}

	unwrapped, err := p.UnwrapDEK(ctx, wrapped, keyID)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}

	for i := range dek {
		if unwrapped[i] != dek[i] {
			t.Fatalf("unwrapped[%d] mismatch", i)
		}
	}
}

func TestFileKeyProvider_DefaultKeyID(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	path := filepath.Join(t.TempDir(), "test.key")
	_ = os.WriteFile(path, key, 0o600)

	p, err := NewFileKeyProvider(path, "")
	if err != nil {
		t.Fatalf("NewFileKeyProvider: %v", err)
	}

	if p.KeyID() != "file-0" {
		t.Errorf("KeyID = %q, want %q", p.KeyID(), "file-0")
	}
}

func TestFileKeyProvider_FileMissing(t *testing.T) {
	t.Parallel()
	_, err := NewFileKeyProvider("/nonexistent/key.file", "test")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestFileKeyProvider_WrongSize(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "test.key")
	_ = os.WriteFile(path, make([]byte, 16), 0o600)

	_, err := NewFileKeyProvider(path, "test")
	if err == nil {
		t.Error("expected error for 16-byte key file")
	}
}

// -------------------------------------------------------------------------
// MULTI-KEY PROVIDER (KEY ROTATION)
// -------------------------------------------------------------------------

func TestMultiKeyProvider_WrapUsePrimary(t *testing.T) {
	t.Parallel()
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	_, _ = rand.Read(key1)
	_, _ = rand.Read(key2)

	primary, _ := NewConfigKeyProvider(base64.StdEncoding.EncodeToString(key1), "primary")
	previous, _ := NewConfigKeyProvider(base64.StdEncoding.EncodeToString(key2), "old")

	multi := NewMultiKeyProvider(primary, []KeyProvider{previous})

	if multi.KeyID() != "primary" {
		t.Errorf("KeyID = %q, want %q", multi.KeyID(), "primary")
	}

	ctx := context.Background()
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)

	_, keyID, err := multi.WrapDEK(ctx, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if keyID != "primary" {
		t.Errorf("WrapDEK used %q, want %q", keyID, "primary")
	}
}

func TestMultiKeyProvider_UnwrapWithOldKey(t *testing.T) {
	t.Parallel()
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	_, _ = rand.Read(key1)
	_, _ = rand.Read(key2)

	primary, _ := NewConfigKeyProvider(base64.StdEncoding.EncodeToString(key1), "primary")
	old, _ := NewConfigKeyProvider(base64.StdEncoding.EncodeToString(key2), "old")

	// Wrap with the old key first
	ctx := context.Background()
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)

	wrapped, _, err := old.WrapDEK(ctx, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}

	// Now create multi-key provider with old as previous
	multi := NewMultiKeyProvider(primary, []KeyProvider{old})

	// Should unwrap using the old key
	unwrapped, err := multi.UnwrapDEK(ctx, wrapped, "old")
	if err != nil {
		t.Fatalf("UnwrapDEK with old key: %v", err)
	}

	for i := range dek {
		if unwrapped[i] != dek[i] {
			t.Fatalf("unwrapped[%d] mismatch", i)
		}
	}
}

func TestMultiKeyProvider_UnwrapWithPrimary(t *testing.T) {
	t.Parallel()
	key1 := make([]byte, 32)
	_, _ = rand.Read(key1)

	primary, _ := NewConfigKeyProvider(base64.StdEncoding.EncodeToString(key1), "primary")
	multi := NewMultiKeyProvider(primary, nil)

	ctx := context.Background()
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)

	wrapped, _, err := multi.WrapDEK(ctx, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}

	unwrapped, err := multi.UnwrapDEK(ctx, wrapped, "primary")
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}

	for i := range dek {
		if unwrapped[i] != dek[i] {
			t.Fatalf("unwrapped[%d] mismatch", i)
		}
	}
}

func TestMultiKeyProvider_UnknownKeyIDReturnsError(t *testing.T) {
	t.Parallel()
	key1 := make([]byte, 32)
	_, _ = rand.Read(key1)

	primary, _ := NewConfigKeyProvider(base64.StdEncoding.EncodeToString(key1), "primary")
	multi := NewMultiKeyProvider(primary, nil)

	ctx := context.Background()
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)

	wrapped, _, _ := primary.WrapDEK(ctx, dek)

	// Unwrap with an unknown keyID — must return an error instead of
	// silently falling back to the primary key.
	before := promtest.ToFloat64(telemetry.EncryptionUnknownKeyIDTotal)
	_, err := multi.UnwrapDEK(ctx, wrapped, "unknown-key-id")
	if err == nil {
		t.Fatal("UnwrapDEK with unknown keyID should return an error")
	}
	if !strings.Contains(err.Error(), "unknown encryption key ID") {
		t.Errorf("error should mention unknown key ID, got: %v", err)
	}

	after := promtest.ToFloat64(telemetry.EncryptionUnknownKeyIDTotal)
	if after != before+1 {
		t.Errorf("EncryptionUnknownKeyIDTotal = %v, want %v", after, before+1)
	}
}

// -------------------------------------------------------------------------
// NEW KEY PROVIDER FROM CONFIG
// -------------------------------------------------------------------------

func TestNewKeyProviderFromConfig_InlineKey(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	b64 := base64.StdEncoding.EncodeToString(key)

	cfg := &config.EncryptionConfig{MasterKey: b64}
	p, err := NewKeyProviderFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewKeyProviderFromConfig: %v", err)
	}
	if p.KeyID() != "config-0" {
		t.Errorf("KeyID = %q, want %q", p.KeyID(), "config-0")
	}
}

func TestNewKeyProviderFromConfig_FileKey(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	path := filepath.Join(t.TempDir(), "test.key")
	_ = os.WriteFile(path, key, 0o600)

	cfg := &config.EncryptionConfig{MasterKeyFile: path}
	p, err := NewKeyProviderFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewKeyProviderFromConfig: %v", err)
	}
	if p.KeyID() != "file-0" {
		t.Errorf("KeyID = %q, want %q", p.KeyID(), "file-0")
	}
}

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

func TestNewKeyProviderFromConfig_NoKeySource(t *testing.T) {
	t.Parallel()
	cfg := &config.EncryptionConfig{}
	_, err := NewKeyProviderFromConfig(cfg)
	if err == nil {
		t.Error("expected error when no key source configured")
	}
}

func TestNewKeyProviderFromConfig_WithPreviousKeys(t *testing.T) {
	t.Parallel()
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	_, _ = rand.Read(key1)
	_, _ = rand.Read(key2)

	cfg := &config.EncryptionConfig{
		MasterKey:    base64.StdEncoding.EncodeToString(key1),
		PreviousKeys: []string{base64.StdEncoding.EncodeToString(key2)},
	}
	p, err := NewKeyProviderFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewKeyProviderFromConfig: %v", err)
	}

	// Should be a MultiKeyProvider wrapping the primary
	if p.KeyID() != "config-0" {
		t.Errorf("KeyID = %q, want %q", p.KeyID(), "config-0")
	}

	// Verify we can wrap and unwrap with the primary
	ctx := context.Background()
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)

	wrapped, _, err := p.WrapDEK(ctx, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	unwrapped, err := p.UnwrapDEK(ctx, wrapped, "config-0")
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	for i := range dek {
		if unwrapped[i] != dek[i] {
			t.Fatalf("unwrapped[%d] mismatch", i)
		}
	}
}

func TestNewKeyProviderFromConfig_InvalidPreviousKey(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	cfg := &config.EncryptionConfig{
		MasterKey:    base64.StdEncoding.EncodeToString(key),
		PreviousKeys: []string{"not-valid-base64!!!"},
	}
	_, err := NewKeyProviderFromConfig(cfg)
	if err == nil {
		t.Error("expected error for invalid previous key")
	}
}

func TestNewKeyProviderFromConfig_InvalidPrimaryKey(t *testing.T) {
	t.Parallel()
	cfg := &config.EncryptionConfig{MasterKey: "not-valid-base64!!!"}
	_, err := NewKeyProviderFromConfig(cfg)
	if err == nil {
		t.Error("expected error for invalid primary key")
	}
}
