// -------------------------------------------------------------------------------
// Vault Transit Key Provider
//
// Author: Alex Freidah
//
// Delegates DEK wrap/unwrap to HashiCorp Vault Transit secrets engine. The
// master key never leaves Vault -- only wrapped DEKs are stored in the
// database. Suitable for Nomad and Kubernetes deployments where Vault is
// available as a cluster service.
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// VaultKeyProvider wraps and unwraps DEKs via the Vault Transit encrypt and
// decrypt endpoints. The Vault token must have permissions for the configured
// transit key.
type VaultKeyProvider struct {
	address   string
	token     string
	keyName   string
	mountPath string
	keyID     string
	client    *http.Client
}

// NewVaultKeyProvider creates a provider backed by Vault Transit. The
// mountPath defaults to "transit" if empty. If caCertPath is non-empty,
// the file is loaded as a PEM CA certificate for TLS verification.
func NewVaultKeyProvider(address, token, keyName, mountPath, caCertPath string) (*VaultKeyProvider, error) {
	if mountPath == "" {
		mountPath = "transit"
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if caCertPath != "" {
		pem, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("read vault CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("vault CA cert contains no valid certificates")
		}
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		}
	}
	return &VaultKeyProvider{
		address:   address,
		token:     token,
		keyName:   keyName,
		mountPath: mountPath,
		keyID:     fmt.Sprintf("vault:%s/%s", mountPath, keyName),
		client:    client,
	}, nil
}

// KeyID returns a composite identifier of the form "vault:{mount}/{key}".
func (p *VaultKeyProvider) KeyID() string { return p.keyID }

// WrapDEK sends the plaintext DEK to Vault Transit for encryption and returns
// the Vault ciphertext blob as the wrapped DEK.
func (p *VaultKeyProvider) WrapDEK(ctx context.Context, dek []byte) ([]byte, string, error) {
	payload := map[string]string{
		"plaintext": base64.StdEncoding.EncodeToString(dek),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal: %w", err)
	}

	url := fmt.Sprintf("%s/v1/%s/encrypt/%s", p.address, p.mountPath, p.keyName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("request: %w", err)
	}
	req.Header.Set("X-Vault-Token", p.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("vault encrypt: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("vault encrypt: status %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}

	return []byte(result.Data.Ciphertext), p.keyID, nil
}

// UnwrapDEK sends a Vault Transit ciphertext blob for decryption and returns
// the recovered plaintext DEK.
func (p *VaultKeyProvider) UnwrapDEK(ctx context.Context, wrappedDEK []byte, _ string) ([]byte, error) {
	payload := map[string]string{
		"ciphertext": string(wrappedDEK),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	url := fmt.Sprintf("%s/v1/%s/decrypt/%s", p.address, p.mountPath, p.keyName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("X-Vault-Token", p.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault decrypt: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vault decrypt: status %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	dek, err := base64.StdEncoding.DecodeString(result.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("decode plaintext: %w", err)
	}
	return dek, nil
}
