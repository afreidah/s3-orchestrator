// -------------------------------------------------------------------------------
// Key Management - Envelope Encryption Key Providers
//
// Author: Alex Freidah
//
// KeyProvider interface and implementations for wrapping and unwrapping
// per-object Data Encryption Keys (DEKs). Each object gets a random 256-bit
// DEK that is wrapped (encrypted) by a master key before storage. Supports
// inline config keys, file-based keys, and key rotation via MultiKeyProvider.
// -------------------------------------------------------------------------------

package encryption

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// -------------------------------------------------------------------------
// KEY PROVIDER INTERFACE
// -------------------------------------------------------------------------

// KeyProvider wraps and unwraps per-object DEKs using a master key. Each
// implementation corresponds to a different key source (config, file, Vault).
type KeyProvider interface {
	// WrapDEK encrypts a plaintext DEK with the master key and returns the
	// wrapped bytes along with the key identifier used for wrapping.
	WrapDEK(ctx context.Context, dek []byte) (wrappedDEK []byte, keyID string, err error)

	// UnwrapDEK decrypts a wrapped DEK using the key identified by keyID.
	UnwrapDEK(ctx context.Context, wrappedDEK []byte, keyID string) (dek []byte, err error)

	// KeyID returns the identifier for the current master key.
	KeyID() string
}

// -------------------------------------------------------------------------
// CONFIG KEY PROVIDER
// -------------------------------------------------------------------------

// ConfigKeyProvider wraps DEKs using an AES-256-GCM key derived from a
// base64-encoded master key supplied in the configuration file or via
// environment variable.
type ConfigKeyProvider struct {
	key   []byte
	keyID string
}

// NewConfigKeyProvider creates a provider from a base64-encoded 256-bit key.
// The keyID identifies this key for rotation tracking; defaults to "config-0".
func NewConfigKeyProvider(masterKeyB64 string, keyID string) (*ConfigKeyProvider, error) {
	key, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 master key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
	}
	if keyID == "" {
		keyID = "config-0"
	}
	return &ConfigKeyProvider{key: key, keyID: keyID}, nil
}

// KeyID returns the identifier for this config-based master key.
func (p *ConfigKeyProvider) KeyID() string { return p.keyID }

// WrapDEK encrypts the DEK with AES-256-GCM using the inline master key.
func (p *ConfigKeyProvider) WrapDEK(_ context.Context, dek []byte) ([]byte, string, error) {
	wrapped, err := aesGCMWrap(p.key, dek)
	if err != nil {
		return nil, "", err
	}
	return wrapped, p.keyID, nil
}

// UnwrapDEK decrypts a wrapped DEK using the inline master key.
func (p *ConfigKeyProvider) UnwrapDEK(_ context.Context, wrappedDEK []byte, _ string) ([]byte, error) {
	return aesGCMUnwrap(p.key, wrappedDEK)
}

// -------------------------------------------------------------------------
// FILE KEY PROVIDER
// -------------------------------------------------------------------------

// FileKeyProvider wraps DEKs using a raw 32-byte key read from a file on
// disk. Suitable for bare-metal and systemd deployments where keys are
// provisioned by configuration management tools.
type FileKeyProvider struct {
	key   []byte
	keyID string
}

// NewFileKeyProvider creates a provider by reading a raw 32-byte key from
// the given file path. The keyID defaults to "file-0" if empty.
func NewFileKeyProvider(path string, keyID string) (*FileKeyProvider, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key file must contain exactly 32 bytes, got %d", len(key))
	}
	if keyID == "" {
		keyID = "file-0"
	}
	return &FileKeyProvider{key: key, keyID: keyID}, nil
}

// KeyID returns the identifier for this file-based master key.
func (p *FileKeyProvider) KeyID() string { return p.keyID }

// WrapDEK encrypts the DEK with AES-256-GCM using the file-based master key.
func (p *FileKeyProvider) WrapDEK(_ context.Context, dek []byte) ([]byte, string, error) {
	wrapped, err := aesGCMWrap(p.key, dek)
	if err != nil {
		return nil, "", err
	}
	return wrapped, p.keyID, nil
}

// UnwrapDEK decrypts a wrapped DEK using the file-based master key.
func (p *FileKeyProvider) UnwrapDEK(_ context.Context, wrappedDEK []byte, _ string) ([]byte, error) {
	return aesGCMUnwrap(p.key, wrappedDEK)
}

// -------------------------------------------------------------------------
// MULTI-KEY PROVIDER (KEY ROTATION)
// -------------------------------------------------------------------------

// MultiKeyProvider wraps a primary provider with fallback providers for key
// rotation. WrapDEK always uses the primary key; UnwrapDEK resolves the
// correct provider by keyID, falling back to previous keys for objects
// encrypted before a rotation.
type MultiKeyProvider struct {
	primary  KeyProvider
	previous map[string]KeyProvider
}

// NewMultiKeyProvider creates a rotation-aware provider. New DEKs are wrapped
// with the primary key; existing DEKs are unwrapped using whichever key
// originally wrapped them.
func NewMultiKeyProvider(primary KeyProvider, previous []KeyProvider) *MultiKeyProvider {
	prev := make(map[string]KeyProvider, len(previous))
	for _, p := range previous {
		prev[p.KeyID()] = p
	}
	return &MultiKeyProvider{primary: primary, previous: prev}
}

// KeyID returns the primary key identifier.
func (p *MultiKeyProvider) KeyID() string { return p.primary.KeyID() }

// WrapDEK encrypts a DEK using the primary (current) master key.
func (p *MultiKeyProvider) WrapDEK(ctx context.Context, dek []byte) ([]byte, string, error) {
	return p.primary.WrapDEK(ctx, dek)
}

// UnwrapDEK decrypts a wrapped DEK by resolving the provider for the given
// keyID. Falls back to the primary provider if the keyID is not found in the
// previous keys map, logging a warning so operators can investigate potential
// metadata corruption or missing rotation keys.
func (p *MultiKeyProvider) UnwrapDEK(ctx context.Context, wrappedDEK []byte, keyID string) ([]byte, error) {
	if keyID == p.primary.KeyID() {
		return p.primary.UnwrapDEK(ctx, wrappedDEK, keyID)
	}
	if prev, ok := p.previous[keyID]; ok {
		return prev.UnwrapDEK(ctx, wrappedDEK, keyID)
	}
	// Unknown keyID — likely metadata corruption or a rotation key that was
	// removed from config. Log a warning and attempt with the primary key;
	// GCM will reject the tag if it's truly the wrong key.
	telemetry.EncryptionUnknownKeyIDTotal.Inc()
	slog.WarnContext(ctx, "Unknown encryption keyID, falling back to primary",
		"unknown_key_id", keyID, "primary_key_id", p.primary.KeyID())
	return p.primary.UnwrapDEK(ctx, wrappedDEK, keyID)
}

// -------------------------------------------------------------------------
// FACTORY
// -------------------------------------------------------------------------

// NewKeyProviderFromConfig creates the appropriate KeyProvider from the
// encryption configuration. Returns a MultiKeyProvider when previous keys
// are configured for rotation support.
func NewKeyProviderFromConfig(cfg *config.EncryptionConfig) (KeyProvider, error) {
	var primary KeyProvider
	var err error

	switch {
	case cfg.MasterKey != "":
		primary, err = NewConfigKeyProvider(cfg.MasterKey, "config-0")
	case cfg.MasterKeyFile != "":
		primary, err = NewFileKeyProvider(cfg.MasterKeyFile, "file-0")
	case cfg.Vault != nil:
		primary, err = NewVaultKeyProvider(cfg.Vault)
	default:
		return nil, fmt.Errorf("no encryption key source configured")
	}
	if err != nil {
		return nil, err
	}

	if len(cfg.PreviousKeys) == 0 {
		return primary, nil
	}

	previous := make([]KeyProvider, 0, len(cfg.PreviousKeys))
	for i, k := range cfg.PreviousKeys {
		p, err := NewConfigKeyProvider(k, fmt.Sprintf("config-%d", i+1))
		if err != nil {
			return nil, fmt.Errorf("previous key %d: %w", i, err)
		}
		previous = append(previous, p)
	}
	return NewMultiKeyProvider(primary, previous), nil
}

// -------------------------------------------------------------------------
// AES-GCM KEY WRAPPING HELPERS
// -------------------------------------------------------------------------

// aesGCMWrap encrypts a DEK with AES-256-GCM using the given master key.
// The output format is nonce (12 bytes) followed by ciphertext with appended
// authentication tag.
func aesGCMWrap(masterKey, dek []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, dek, nil), nil
}

// aesGCMUnwrap decrypts a wrapped DEK produced by aesGCMWrap. Expects the
// nonce prepended to the ciphertext.
func aesGCMUnwrap(masterKey, wrapped []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(wrapped) < nonceSize {
		return nil, fmt.Errorf("wrapped DEK too short")
	}
	return gcm.Open(nil, wrapped[:nonceSize], wrapped[nonceSize:], nil)
}
