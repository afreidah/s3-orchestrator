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
	Enabled       bool              `yaml:"enabled"`
	ChunkSize     int               `yaml:"chunk_size"`      // Plaintext bytes per chunk (default: 65536, range: 4KB-1MB, must be power of 2)
	MasterKey     string            `yaml:"master_key"`      // Base64-encoded 256-bit key (inline or via env var)
	MasterKeyFile string            `yaml:"master_key_file"` // Path to file containing raw 32-byte key
	Vault         *VaultTransitConfig `yaml:"vault"`          // Vault Transit key management
	PreviousKeys  []string          `yaml:"previous_keys"`   // Base64-encoded previous master keys for rotation (unwrap only)
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
	errs = append(errs, validateMasterKey(e.MasterKey)...)
	errs = append(errs, validateMasterKeyFile(e.MasterKeyFile)...)
	errs = append(errs, validatePreviousKeys(e.PreviousKeys)...)
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

// validateMasterKey decodes the inline base64 master key and checks its
// length. Returns nil when the field is unset (another source provides the
// key material).
func validateMasterKey(masterKey string) []error {
	if masterKey == "" {
		return nil
	}
	keyBytes, err := base64.StdEncoding.DecodeString(masterKey)
	if err != nil {
		return []error{fmt.Errorf("encryption.master_key: %w: %v", ErrInvalidBase64Key, err)}
	}
	if len(keyBytes) != 32 {
		return []error{fmt.Errorf("encryption.master_key: %w: got %d bytes", ErrInvalidKeyLength, len(keyBytes))}
	}
	return nil
}

// validateMasterKeyFile stats the configured key file and checks it's the
// expected 32 bytes. Returns nil when the field is unset.
func validateMasterKeyFile(path string) []error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return []error{fmt.Errorf("%w: %v", ErrInvalidKeyFile, err)}
	}
	if info.Size() != 32 {
		return []error{fmt.Errorf("encryption.master_key_file: %w: got %d bytes", ErrInvalidKeyLength, info.Size())}
	}
	return nil
}

// validatePreviousKeys decodes each previous-key entry and checks length.
// Used for rotation: previous keys must be valid so objects encrypted under
// them can still be decrypted.
func validatePreviousKeys(keys []string) []error {
	var errs []error
	for i, pk := range keys {
		keyBytes, err := base64.StdEncoding.DecodeString(pk)
		if err != nil {
			errs = append(errs, fmt.Errorf("encryption.previous_keys[%d]: %w: %v", i, ErrPreviousKeyInvalid, err))
			continue
		}
		if len(keyBytes) != 32 {
			errs = append(errs, fmt.Errorf("encryption.previous_keys[%d]: %w: got %d bytes", i, ErrPreviousKeyInvalid, len(keyBytes)))
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
