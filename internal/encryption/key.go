// -------------------------------------------------------------------------------
// Key Management - Envelope Encryption Key Providers
//
// Author: Alex Freidah
//
// KeyProvider interface and implementations for wrapping and unwrapping
// per-object Data Encryption Keys (DEKs). Each object gets a random 256-bit
// DEK that is wrapped (encrypted) by a master key before storage. Inline and
// file keys share one provider and are recorded under their fingerprint, so a
// key keeps its ID across a rotation; MultiKeyProvider holds the previous keys.
// -------------------------------------------------------------------------------

package encryption

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// KEY PROVIDER INTERFACE
// -------------------------------------------------------------------------

// KeyProvider wraps and unwraps per-object DEKs using a master key. Each
// implementation corresponds to a different key source (config, file, Vault).
// WrapDEK returns the wrapping key's ID so UnwrapDEK can find it after
// rotation, when several keys are held at once.
type KeyProvider interface {
	WrapDEK(ctx context.Context, dek []byte) (wrappedDEK []byte, keyID string, err error)
	UnwrapDEK(ctx context.Context, wrappedDEK []byte, keyID string) (dek []byte, err error)
	KeyID() string // the current master key
}

// -------------------------------------------------------------------------
// STATIC KEY PROVIDER
// -------------------------------------------------------------------------

// StaticKeyProvider wraps DEKs with AES-256-GCM under a master key held in
// memory, whether it came inline from the config or from a key file. Its ID is
// the key's fingerprint, so the same key has the same ID wherever it is
// configured.
type StaticKeyProvider struct {
	key   []byte
	keyID string
}

// NewStaticKeyProvider creates a provider for a 256-bit master key.
func NewStaticKeyProvider(key []byte) (*StaticKeyProvider, error) {
	if len(key) != config.KeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", config.KeySize, len(key))
	}
	return &StaticKeyProvider{key: key, keyID: Fingerprint(key)}, nil
}

// fingerprintLabel separates the fingerprint from any other use of the key.
const fingerprintLabel = "s3-orchestrator key id"

// Fingerprint returns the ID a master key is recorded under: "k-" and the
// first 8 bytes of HMAC-SHA256 over a fixed label, keyed by the key. It names
// the key without revealing anything usable about it.
func Fingerprint(key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(fingerprintLabel))
	return "k-" + hex.EncodeToString(mac.Sum(nil)[:8])
}

// KeyID returns the key's fingerprint.
func (p *StaticKeyProvider) KeyID() string { return p.keyID }

// WrapDEK encrypts the DEK with AES-256-GCM under the master key.
func (p *StaticKeyProvider) WrapDEK(_ context.Context, dek []byte) ([]byte, string, error) {
	wrapped, err := aesGCMWrap(p.key, dek)
	if err != nil {
		return nil, "", err
	}
	return wrapped, p.keyID, nil
}

// UnwrapDEK decrypts a wrapped DEK under the master key. The ID is not
// consulted: GCM authentication rejects a DEK another key wrapped.
func (p *StaticKeyProvider) UnwrapDEK(_ context.Context, wrappedDEK []byte, _ string) ([]byte, error) {
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
	ordered  []KeyProvider
}

// NewMultiKeyProvider creates a rotation-aware provider. New DEKs are wrapped
// with the primary key; existing DEKs are unwrapped using whichever key
// originally wrapped them.
func NewMultiKeyProvider(primary KeyProvider, previous []KeyProvider) *MultiKeyProvider {
	prev := make(map[string]KeyProvider, len(previous))
	for _, p := range previous {
		prev[p.KeyID()] = p
	}
	return &MultiKeyProvider{primary: primary, previous: prev, ordered: append([]KeyProvider{primary}, previous...)}
}

// KeyID returns the primary key identifier.
func (p *MultiKeyProvider) KeyID() string { return p.primary.KeyID() }

// WrapDEK encrypts a DEK using the primary (current) master key.
func (p *MultiKeyProvider) WrapDEK(ctx context.Context, dek []byte) ([]byte, string, error) {
	return p.primary.WrapDEK(ctx, dek)
}

// UnwrapDEK decrypts a wrapped DEK by resolving the provider for the given
// keyID. An ID no configured key carries, such as the positional "config-0"
// and "file-0" earlier releases recorded, is tried against every key in turn;
// GCM authentication rejects each key that did not wrap it. Returns an error
// when none does, which means the key that wrapped it is no longer configured.
func (p *MultiKeyProvider) UnwrapDEK(ctx context.Context, wrappedDEK []byte, keyID string) ([]byte, error) {
	if keyID == p.primary.KeyID() {
		return p.primary.UnwrapDEK(ctx, wrappedDEK, keyID)
	}
	if prev, ok := p.previous[keyID]; ok {
		return prev.UnwrapDEK(ctx, wrappedDEK, keyID)
	}
	for _, candidate := range p.ordered {
		if dek, err := candidate.UnwrapDEK(ctx, wrappedDEK, keyID); err == nil {
			return dek, nil
		}
	}
	telemetry.EncryptionUnknownKeyIDTotal.Inc()
	slog.ErrorContext(ctx, "unknown encryption keyID - object cannot be decrypted",
		logfmt.Component("encryption"),
		"unknown_key_id", keyID,
		"primary_key_id", p.primary.KeyID())
	return nil, fmt.Errorf("no configured key unwraps key ID %q: ensure previous_keys or previous_key_files includes every rotated key", keyID)
}

// -------------------------------------------------------------------------
// FACTORY
// -------------------------------------------------------------------------

// NewKeyProviderFromConfig creates the appropriate KeyProvider from the
// encryption configuration. Returns a MultiKeyProvider when previous keys
// are configured for rotation support.
func NewKeyProviderFromConfig(cfg *config.EncryptionConfig) (KeyProvider, error) {
	primary, err := primaryKeyProvider(cfg)
	if err != nil {
		return nil, err
	}
	var previous []KeyProvider
	for _, src := range []struct {
		field   string
		sources []string
		load    func(string) ([]byte, error)
	}{
		{"previous_keys", cfg.PreviousKeys, config.DecodeKey},
		{"previous_key_files", cfg.PreviousKeyFiles, config.ReadKeyFile},
	} {
		for i, source := range src.sources {
			p, err := staticKeyFrom(source, src.load)
			if err != nil {
				return nil, fmt.Errorf("%s[%d]: %w", src.field, i, err)
			}
			previous = append(previous, p)
		}
	}
	if len(previous) == 0 {
		return primary, nil
	}
	return NewMultiKeyProvider(primary, previous), nil
}

// primaryKeyProvider builds the provider for the one key source the config
// names, which new DEKs are wrapped with.
func primaryKeyProvider(cfg *config.EncryptionConfig) (KeyProvider, error) {
	switch {
	case cfg.MasterKey != "":
		return staticKeyFrom(cfg.MasterKey, config.DecodeKey)
	case cfg.MasterKeyFile != "":
		return staticKeyFrom(cfg.MasterKeyFile, config.ReadKeyFile)
	case cfg.Vault != nil:
		return NewVaultKeyProvider(cfg.Vault)
	}
	return nil, errors.New("no encryption key source configured")
}

// staticKeyFrom loads a master key from source with load, the loader config
// validation already ran it through, and wraps it in a StaticKeyProvider.
func staticKeyFrom(source string, load func(string) ([]byte, error)) (KeyProvider, error) {
	key, err := load(source)
	if err != nil {
		return nil, err
	}
	return NewStaticKeyProvider(key)
}

// -------------------------------------------------------------------------
// AES-GCM KEY WRAPPING HELPERS
// -------------------------------------------------------------------------

// aesGCMWrap encrypts a DEK with AES-256-GCM using the given master key.
// The output format is nonce (12 bytes) followed by ciphertext with appended
// authentication tag.
func aesGCMWrap(masterKey, dek []byte) ([]byte, error) {
	gcm, err := newGCM(masterKey)
	if err != nil {
		return nil, err
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
	gcm, err := newGCM(masterKey)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(wrapped) < nonceSize {
		return nil, fmt.Errorf("wrapped DEK too short")
	}
	return gcm.Open(nil, wrapped[:nonceSize], wrapped[nonceSize:], nil)
}

// newGCM builds the AES-256-GCM AEAD for key, the one cipher both the DEK
// wrapping and the chunk encryption use.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return gcm, nil
}
