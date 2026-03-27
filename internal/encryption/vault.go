// -------------------------------------------------------------------------------
// Vault Transit Key Provider
//
// Author: Alex Freidah
//
// Delegates DEK wrap/unwrap to HashiCorp Vault Transit secrets engine. The
// master key never leaves Vault -- only wrapped DEKs are stored in the
// database. Supports automatic token renewal and token file reloading for
// Nomad workload identity deployments.
// -------------------------------------------------------------------------------

package encryption

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	vault "github.com/hashicorp/vault/api"
)

// VaultKeyProvider wraps and unwraps DEKs via the Vault Transit encrypt and
// decrypt endpoints. A background goroutine renews the token before expiry
// (static token mode) or re-reads it from a file (Nomad workload identity
// mode). The provider is safe for concurrent use.
type VaultKeyProvider struct {
	client        *vault.Client
	keyName       string
	mountPath     string
	keyID         string
	tokenFile     string
	renewInterval time.Duration
	mu            sync.RWMutex
	cancel        context.CancelFunc
}

// NewVaultKeyProvider creates a provider backed by Vault Transit. A background
// goroutine manages token lifecycle: for token_file configs it re-reads the
// file each tick; for static tokens it calls RenewSelf. Call Close to stop
// the renewal goroutine.
func NewVaultKeyProvider(cfg *config.VaultTransitConfig) (*VaultKeyProvider, error) {
	vaultCfg := vault.DefaultConfig()
	vaultCfg.Address = cfg.Address
	vaultCfg.Timeout = 10 * time.Second

	if cfg.CACert != "" {
		if err := vaultCfg.ConfigureTLS(&vault.TLSConfig{CACert: cfg.CACert}); err != nil {
			return nil, fmt.Errorf("vault TLS config: %w", err)
		}
	}

	client, err := vault.NewClient(vaultCfg)
	if err != nil {
		return nil, fmt.Errorf("vault client: %w", err)
	}

	// Set the initial token from config or file.
	if cfg.TokenFile != "" {
		token, err := readTokenFile(cfg.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("initial token load: %w", err)
		}
		client.SetToken(token)
	} else {
		client.SetToken(cfg.Token)
	}

	renewInterval := cfg.RenewInterval
	if renewInterval <= 0 {
		renewInterval = 5 * time.Minute
	}

	ctx, cancel := context.WithCancel(context.Background())
	p := &VaultKeyProvider{
		client:        client,
		keyName:       cfg.KeyName,
		mountPath:     cfg.MountPath,
		keyID:         fmt.Sprintf("vault:%s/%s", cfg.MountPath, cfg.KeyName),
		tokenFile:     cfg.TokenFile,
		renewInterval: renewInterval,
		cancel:        cancel,
	}

	go p.tokenRenewalLoop(ctx)
	return p, nil
}

// KeyID returns a composite identifier of the form "vault:{mount}/{key}".
func (p *VaultKeyProvider) KeyID() string { return p.keyID }

// Close stops the background token renewal goroutine.
func (p *VaultKeyProvider) Close() { p.cancel() }

// WrapDEK sends the plaintext DEK to Vault Transit for encryption and returns
// the Vault ciphertext blob as the wrapped DEK.
func (p *VaultKeyProvider) WrapDEK(ctx context.Context, dek []byte) ([]byte, string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	path := fmt.Sprintf("%s/encrypt/%s", p.mountPath, p.keyName)
	secret, err := p.client.Logical().WriteWithContext(ctx, path, map[string]interface{}{
		"plaintext": base64.StdEncoding.EncodeToString(dek),
	})
	if err != nil {
		return nil, "", fmt.Errorf("vault encrypt: %w", err)
	}
	if secret == nil || secret.Data == nil {
		return nil, "", fmt.Errorf("vault encrypt: empty response")
	}

	ciphertext, ok := secret.Data["ciphertext"].(string)
	if !ok || ciphertext == "" {
		return nil, "", fmt.Errorf("vault encrypt: ciphertext not found in response")
	}
	return []byte(ciphertext), p.keyID, nil
}

// UnwrapDEK sends a Vault Transit ciphertext blob for decryption and returns
// the recovered plaintext DEK.
func (p *VaultKeyProvider) UnwrapDEK(ctx context.Context, wrappedDEK []byte, _ string) ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	path := fmt.Sprintf("%s/decrypt/%s", p.mountPath, p.keyName)
	secret, err := p.client.Logical().WriteWithContext(ctx, path, map[string]interface{}{
		"ciphertext": string(wrappedDEK),
	})
	if err != nil {
		return nil, fmt.Errorf("vault decrypt: %w", err)
	}
	if secret == nil || secret.Data == nil {
		return nil, fmt.Errorf("vault decrypt: empty response")
	}

	plaintext, ok := secret.Data["plaintext"].(string)
	if !ok || plaintext == "" {
		return nil, fmt.Errorf("vault decrypt: plaintext not found in response")
	}

	dek, err := base64.StdEncoding.DecodeString(plaintext)
	if err != nil {
		return nil, fmt.Errorf("vault decrypt: decode plaintext: %w", err)
	}
	return dek, nil
}

// tokenRenewalLoop runs in a background goroutine and keeps the Vault token
// alive. For token_file configs it re-reads the file; for static tokens it
// calls RenewSelf. Modeled on vault-cert-manager's renewal loop.
func (p *VaultKeyProvider) tokenRenewalLoop(ctx context.Context) {
	ticker := time.NewTicker(p.renewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if p.tokenFile != "" {
				if err := p.reloadTokenFile(ctx); err != nil {
					slog.ErrorContext(ctx, "Failed to reload Vault token file",
						"error", err, "path", p.tokenFile)
				}
			} else {
				if err := p.renewToken(ctx); err != nil {
					slog.ErrorContext(ctx, "Failed to renew Vault token", "error", err)
				}
			}
		}
	}
}

// renewToken attempts to extend the current token's TTL via Vault's
// token/renew-self endpoint. The network call runs without the mutex
// held so concurrent WrapDEK/UnwrapDEK calls are not blocked by slow
// or unresponsive Vault instances.
func (p *VaultKeyProvider) renewToken(ctx context.Context) error {
	// Read the current client reference under the read lock. The Vault
	// client is safe for concurrent use; only SetToken mutates state.
	p.mu.RLock()
	client := p.client
	p.mu.RUnlock()

	secret, err := client.Auth().Token().RenewSelf(0)
	if err != nil {
		return fmt.Errorf("token renewal failed: %w", err)
	}
	if secret == nil || secret.Auth == nil {
		return fmt.Errorf("empty response from token renewal")
	}
	slog.InfoContext(ctx, "Vault token renewed", "ttl", secret.Auth.LeaseDuration)
	return nil
}

// reloadTokenFile reads a fresh token from the configured file path and
// updates the Vault client. Nomad keeps this file current when using
// workload identity.
func (p *VaultKeyProvider) reloadTokenFile(ctx context.Context) error {
	token, err := readTokenFile(p.tokenFile)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client.SetToken(token)
	slog.DebugContext(ctx, "Vault token reloaded from file", "path", p.tokenFile)
	return nil
}

// readTokenFile reads and trims a token from a file path. Returns an error
// if the file is group- or world-readable (permissions wider than 0600) to
// prevent accidental exposure of the Vault token to other local users.
func readTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat token file %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("token file %s has insecure permissions %04o (must be 0600 or stricter)", path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}
