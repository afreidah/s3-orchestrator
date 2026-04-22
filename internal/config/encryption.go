// -------------------------------------------------------------------------------
// Encryption Configuration
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"time"
)

// EncryptionConfig holds settings for server-side envelope encryption.
// When enabled, objects are encrypted with per-object DEKs using chunked
// AES-256-GCM before being stored on backends. Exactly one key source
// (master_key, master_key_file, or vault) must be configured.
type EncryptionConfig struct {
	Enabled       bool              `yaml:"enabled"`
	ChunkSize     int               `yaml:"chunk_size"`      // Plaintext bytes per chunk (default: 65536, range: 4KB–1MB, must be power of 2)
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

func (e *EncryptionConfig) setDefaultsAndValidate() []error {
	if !e.Enabled {
		return nil
	}

	var errs []error

	if e.ChunkSize == 0 {
		e.ChunkSize = 65536
	}
	cs := e.ChunkSize
	if cs < 4096 || cs > 1048576 {
		errs = append(errs, ErrInvalidChunkSize)
	} else if cs&(cs-1) != 0 {
		errs = append(errs, ErrChunkSizeNotPowerOf2)
	}

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
	if sources == 0 {
		errs = append(errs, ErrKeySourceRequired)
	} else if sources > 1 {
		errs = append(errs, ErrMultipleKeySources)
	}

	if e.MasterKey != "" {
		keyBytes, err := base64.StdEncoding.DecodeString(e.MasterKey)
		if err != nil {
			errs = append(errs, fmt.Errorf("encryption.master_key: %w: %v", ErrInvalidBase64Key, err))
		} else if len(keyBytes) != 32 {
			errs = append(errs, fmt.Errorf("encryption.master_key: %w: got %d bytes", ErrInvalidKeyLength, len(keyBytes)))
		}
	}

	if e.MasterKeyFile != "" {
		info, err := os.Stat(e.MasterKeyFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("%w: %v", ErrInvalidKeyFile, err))
		} else if info.Size() != 32 {
			errs = append(errs, fmt.Errorf("encryption.master_key_file: %w: got %d bytes", ErrInvalidKeyLength, info.Size()))
		}
	}

	for i, pk := range e.PreviousKeys {
		keyBytes, err := base64.StdEncoding.DecodeString(pk)
		if err != nil {
			errs = append(errs, fmt.Errorf("encryption.previous_keys[%d]: %w: %v", i, ErrPreviousKeyInvalid, err))
		} else if len(keyBytes) != 32 {
			errs = append(errs, fmt.Errorf("encryption.previous_keys[%d]: %w: got %d bytes", i, ErrPreviousKeyInvalid, len(keyBytes)))
		}
	}

	if e.Vault != nil {
		errs = append(errs, e.Vault.setDefaultsAndValidate()...)
	}

	return errs
}

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
	if v.MountPath == "" {
		v.MountPath = "transit"
	}
	if v.RenewInterval == 0 {
		v.RenewInterval = 5 * time.Minute
	}

	return errs
}
