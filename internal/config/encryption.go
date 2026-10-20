// -------------------------------------------------------------------------------
// Encryption Configuration
//
// Author: Alex Freidah
//
// Defines EncryptionConfig: the master key source (env, file, or vault),
// the chunk size used by the streaming encryptor, and the validator that
// enforces the "exactly one key source" fan-in rule. The fan-in guard
// prevents ambiguous key material from silently picking the wrong source
// when multiple are partially configured. Master keys are base64-decoded
// at validation time so a malformed key fails startup, not the first PUT.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"encoding/base64"
	"fmt"
	"os"
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// EncryptionConfig holds settings for server-side envelope encryption.
// When enabled, objects are encrypted with per-object DEKs using chunked
// AES-256-GCM before being stored on backends. Exactly one key source
// (master_key, master_key_file, or vault) must be configured.
type EncryptionConfig struct {
	Enabled          bool                `yaml:"enabled"`
	ChunkSize        int                 `yaml:"chunk_size"`         // Plaintext bytes per chunk (default: 65536, range: 4KB-1MB, must be power of 2)
	MasterKey        string              `yaml:"master_key"`         // Base64-encoded 256-bit key (inline or via env var)
	MasterKeyFile    string              `yaml:"master_key_file"`    // Path to file containing raw 32-byte key
	Vault            *VaultTransitConfig `yaml:"vault"`              // Vault Transit key management
	PreviousKeys     []string            `yaml:"previous_keys"`      // Base64-encoded previous master keys for rotation (unwrap only)
	PreviousKeyFiles []string            `yaml:"previous_key_files"` // Paths to raw 32-byte previous master keys for rotation (unwrap only)
}

// KeySize is the length of every master key, AES-256.
const KeySize = 32

// DecodeKey decodes a base64 master key and checks it is KeySize bytes.
// Validation and the key providers share it, so a key that passes startup
// is the key the providers load.
func DecodeKey(b64 string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidBase64Key, err)
	}
	return checkKeySize(key)
}

// ReadKeyFile reads a raw master key from path and checks it is KeySize
// bytes, the file form of DecodeKey.
func ReadKeyFile(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidKeyFile, err)
	}
	return checkKeySize(key)
}

// checkKeySize returns key when it is KeySize bytes and an error naming its
// length otherwise.
func checkKeySize(key []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeyLength, len(key))
	}
	return key, nil
}

// VaultTransitConfig holds settings for HashiCorp Vault Transit key management.
type VaultTransitConfig struct {
	Address       string        `yaml:"address"`        // Vault server URL
	Token         string        `yaml:"token"`          // Vault token (or via env var)
	TokenFile     string        `yaml:"token_file"`     // Path to file containing Vault token (re-read on each renewal tick; for Nomad workload identity)
	KeyName       string        `yaml:"key_name"`       // Transit key name
	MountPath     string        `yaml:"mount_path"`     // Transit mount path (default: "transit")
	CACert        string        `yaml:"ca_cert"`        // Path to PEM CA certificate for TLS verification
	RenewInterval time.Duration `yaml:"renew_interval"` // Token renewal check interval (default: 5m)
}

// -------------------------------------------------------------------------
// VALIDATION
// -------------------------------------------------------------------------

// setDefaultsAndValidate sets defaults and validate.
func (e *EncryptionConfig) setDefaultsAndValidate() []error {
	if !e.Enabled {
		return nil
	}
	var errs []error
	errs = append(errs, e.validateChunkSize()...)
	errs = append(errs, e.validateKeySources()...)
	errs = append(errs, validateKey("encryption.master_key", e.MasterKey, DecodeKey)...)
	errs = append(errs, validateKey("encryption.master_key_file", e.MasterKeyFile, ReadKeyFile)...)
	errs = append(errs, validatePreviousKeys("previous_keys", e.PreviousKeys, DecodeKey)...)
	errs = append(errs, validatePreviousKeys("previous_key_files", e.PreviousKeyFiles, ReadKeyFile)...)
	if e.Vault != nil {
		errs = append(errs, e.Vault.setDefaultsAndValidate()...)
	}
	return errs
}

// validateChunkSize applies the ChunkSize default and enforces the
// 4KB-1MB-power-of-two constraints.
func (e *EncryptionConfig) validateChunkSize() []error {
	e.ChunkSize = cmp.Or(e.ChunkSize, 65536)
	cs := e.ChunkSize
	if cs < 4096 || cs > 1048576 {
		return []error{ErrInvalidChunkSize}
	}
	if cs&(cs-1) != 0 {
		return []error{ErrChunkSizeNotPowerOf2}
	}
	return nil
}

// validateKeySources enforces "exactly one of master_key, master_key_file,
// or vault"  -  the fan-in rule that prevents ambiguous key material.
func (e *EncryptionConfig) validateKeySources() []error {
	sources := 0
	if e.MasterKey != "" {
		sources++
	}
	if e.MasterKeyFile != "" {
		sources++
	}
	if e.Vault != nil {
		sources++
	}
	switch {
	case sources == 0:
		return []error{ErrKeySourceRequired}
	case sources > 1:
		return []error{ErrMultipleKeySources}
	}
	return nil
}

// validateKey loads one key source with load and reports why it is unusable.
// Returns nil when the field is unset, since another source provides the key.
func validateKey(field, source string, load func(string) ([]byte, error)) []error {
	if source == "" {
		return nil
	}
	if _, err := load(source); err != nil {
		return []error{fmt.Errorf("%s: %w", field, err)}
	}
	return nil
}

// validatePreviousKeys loads each previous key with load. They must all be
// usable, since objects wrapped under them are unwrapped with them.
func validatePreviousKeys(field string, sources []string, load func(string) ([]byte, error)) []error {
	var errs []error
	for i, source := range sources {
		if _, err := load(source); err != nil {
			errs = append(errs, fmt.Errorf("encryption.%s[%d]: %w: %w", field, i, ErrPreviousKeyInvalid, err))
		}
	}
	return errs
}

// setDefaultsAndValidate sets defaults and validate.
func (v *VaultTransitConfig) setDefaultsAndValidate() []error {
	var errs []error

	if v.Address == "" {
		errs = append(errs, ErrVaultAddressRequired)
	}
	if v.Token == "" && v.TokenFile == "" {
		errs = append(errs, ErrVaultTokenRequired)
	}
	if v.Token != "" && v.TokenFile != "" {
		errs = append(errs, ErrVaultTokenAmbiguous)
	}
	if v.KeyName == "" {
		errs = append(errs, ErrVaultKeyNameRequired)
	}
	v.MountPath = cmp.Or(v.MountPath, "transit")
	v.RenewInterval = cmp.Or(v.RenewInterval, 5*time.Minute)

	return errs
}
