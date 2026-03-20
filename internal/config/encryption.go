// -------------------------------------------------------------------------------
// Encryption Configuration
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package config

import (
	"encoding/base64"
	"fmt"
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

func (e *EncryptionConfig) setDefaultsAndValidate() []string {
	if !e.Enabled {
		return nil
	}

	var errs []string

	if e.ChunkSize == 0 {
		e.ChunkSize = 65536
	}
	cs := e.ChunkSize
	if cs < 4096 || cs > 1048576 {
		errs = append(errs, "encryption.chunk_size must be between 4096 (4KB) and 1048576 (1MB)")
	} else if cs&(cs-1) != 0 {
		errs = append(errs, "encryption.chunk_size must be a power of 2")
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
		errs = append(errs, "encryption: exactly one of master_key, master_key_file, or vault is required")
	} else if sources > 1 {
		errs = append(errs, "encryption: only one of master_key, master_key_file, or vault may be set")
	}

	if e.MasterKey != "" {
		keyBytes, err := base64.StdEncoding.DecodeString(e.MasterKey)
		if err != nil {
			errs = append(errs, fmt.Sprintf("encryption.master_key: invalid base64: %v", err))
		} else if len(keyBytes) != 32 {
			errs = append(errs, fmt.Sprintf("encryption.master_key: must be 256 bits (32 bytes), got %d bytes", len(keyBytes)))
		}
	}

	for i, pk := range e.PreviousKeys {
		keyBytes, err := base64.StdEncoding.DecodeString(pk)
		if err != nil {
			errs = append(errs, fmt.Sprintf("encryption.previous_keys[%d]: invalid base64: %v", i, err))
		} else if len(keyBytes) != 32 {
			errs = append(errs, fmt.Sprintf("encryption.previous_keys[%d]: must be 256 bits (32 bytes), got %d bytes", i, len(keyBytes)))
		}
	}

	if e.Vault != nil {
		errs = append(errs, e.Vault.setDefaultsAndValidate()...)
	}

	return errs
}

func (v *VaultTransitConfig) setDefaultsAndValidate() []string {
	var errs []string

	if v.Address == "" {
		errs = append(errs, "encryption.vault.address is required")
	}
	if v.Token == "" && v.TokenFile == "" {
		errs = append(errs, "encryption.vault: one of token or token_file is required")
	}
	if v.Token != "" && v.TokenFile != "" {
		errs = append(errs, "encryption.vault: only one of token or token_file may be set")
	}
	if v.KeyName == "" {
		errs = append(errs, "encryption.vault.key_name is required")
	}
	if v.MountPath == "" {
		v.MountPath = "transit"
	}
	if v.RenewInterval == 0 {
		v.RenewInterval = 5 * time.Minute
	}

	return errs
}
