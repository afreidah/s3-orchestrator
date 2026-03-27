// -------------------------------------------------------------------------------
// Vault Transit Key Provider Tests
//
// Author: Alex Freidah
//
// Tests for VaultKeyProvider: token renewal, token file reload, WrapDEK/UnwrapDEK
// round-trips against a fake Vault server, and graceful shutdown via Close.
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// newFakeVault creates an httptest server that mimics Vault Transit encrypt,
// decrypt, and token renew-self endpoints.
func newFakeVault(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/transit/encrypt/test-key", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Plaintext string `json:"plaintext"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Echo the plaintext back as "ciphertext" for round-trip testing.
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]string{"ciphertext": "vault:v1:" + req.Plaintext},
		})
	})

	mux.HandleFunc("/v1/transit/decrypt/test-key", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Ciphertext string `json:"ciphertext"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Strip the "vault:v1:" prefix to recover the original base64 plaintext.
		const prefix = "vault:v1:"
		if len(req.Ciphertext) < len(prefix) {
			http.Error(w, "bad ciphertext", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]string{"plaintext": req.Ciphertext[len(prefix):]},
		})
	})

	mux.HandleFunc("/v1/auth/token/renew-self", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"auth": map[string]interface{}{
				"client_token":   "renewed-token",
				"lease_duration": 3600,
				"renewable":      true,
			},
		})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newTestVaultProvider(t *testing.T, ts *httptest.Server) *VaultKeyProvider {
	t.Helper()
	p, err := NewVaultKeyProvider(&config.VaultTransitConfig{
		Address:   ts.URL,
		Token:     "test-token",
		KeyName:   "test-key",
		MountPath: "transit",
	})
	if err != nil {
		t.Fatalf("NewVaultKeyProvider: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestVaultKeyProvider_WrapUnwrapRoundTrip(t *testing.T) {
	ts := newFakeVault(t)
	p := newTestVaultProvider(t, ts)

	original := []byte("this-is-a-32-byte-test-dek-value")
	wrapped, keyID, err := p.WrapDEK(context.Background(), original)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if keyID != "vault:transit/test-key" {
		t.Errorf("keyID = %q, want %q", keyID, "vault:transit/test-key")
	}

	unwrapped, err := p.UnwrapDEK(context.Background(), wrapped, keyID)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(unwrapped, original) {
		t.Errorf("round-trip failed: got %q, want %q", unwrapped, original)
	}
}

func TestVaultKeyProvider_WrapDEK_ErrorResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/transit/encrypt/test-key", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "permission denied", http.StatusForbidden)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	p, err := NewVaultKeyProvider(&config.VaultTransitConfig{
		Address:   ts.URL,
		Token:     "bad-token",
		KeyName:   "test-key",
		MountPath: "transit",
	})
	if err != nil {
		t.Fatalf("NewVaultKeyProvider: %v", err)
	}
	t.Cleanup(p.Close)

	_, _, err = p.WrapDEK(context.Background(), []byte("dek"))
	if err == nil {
		t.Fatal("expected error from 403 response")
	}
}

func TestVaultKeyProvider_UnwrapDEK_ErrorResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/transit/decrypt/test-key", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "permission denied", http.StatusForbidden)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	p, err := NewVaultKeyProvider(&config.VaultTransitConfig{
		Address:   ts.URL,
		Token:     "bad-token",
		KeyName:   "test-key",
		MountPath: "transit",
	})
	if err != nil {
		t.Fatalf("NewVaultKeyProvider: %v", err)
	}
	t.Cleanup(p.Close)

	_, err = p.UnwrapDEK(context.Background(), []byte("vault:v1:"+base64.StdEncoding.EncodeToString([]byte("dek"))), "")
	if err == nil {
		t.Fatal("expected error from 403 response")
	}
}

func TestVaultKeyProvider_RenewToken(t *testing.T) {
	var renewCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token/renew-self", func(w http.ResponseWriter, _ *http.Request) {
		renewCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"auth": map[string]interface{}{
				"client_token":   "renewed",
				"lease_duration": 3600,
				"renewable":      true,
			},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	p, err := NewVaultKeyProvider(&config.VaultTransitConfig{
		Address:       ts.URL,
		Token:         "initial-token",
		KeyName:       "test-key",
		MountPath:     "transit",
		RenewInterval: 50 * time.Millisecond, // fast tick for testing
	})
	if err != nil {
		t.Fatalf("NewVaultKeyProvider: %v", err)
	}
	t.Cleanup(p.Close)

	// Wait for at least one renewal tick
	time.Sleep(150 * time.Millisecond)

	if got := renewCalls.Load(); got == 0 {
		t.Error("expected at least one RenewSelf call")
	}
}

func TestVaultKeyProvider_TokenFile(t *testing.T) {
	ts := newFakeVault(t)

	tokenFile := filepath.Join(t.TempDir(), "vault-token")
	if err := os.WriteFile(tokenFile, []byte("initial-file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := NewVaultKeyProvider(&config.VaultTransitConfig{
		Address:       ts.URL,
		TokenFile:     tokenFile,
		KeyName:       "test-key",
		MountPath:     "transit",
		RenewInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewVaultKeyProvider: %v", err)
	}
	t.Cleanup(p.Close)

	// Verify the initial token was loaded
	p.mu.RLock()
	initialToken := p.client.Token()
	p.mu.RUnlock()
	if initialToken != "initial-file-token" {
		t.Errorf("initial token = %q, want %q", initialToken, "initial-file-token")
	}

	// Update the file and wait for reload
	if err := os.WriteFile(tokenFile, []byte("updated-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	p.mu.RLock()
	updatedToken := p.client.Token()
	p.mu.RUnlock()
	if updatedToken != "updated-token" {
		t.Errorf("updated token = %q, want %q", updatedToken, "updated-token")
	}
}

func TestVaultKeyProvider_TokenFileEmpty(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "empty-token")
	if err := os.WriteFile(tokenFile, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := NewVaultKeyProvider(&config.VaultTransitConfig{
		Address:   "https://vault.example.com",
		TokenFile: tokenFile,
		KeyName:   "test-key",
		MountPath: "transit",
	})
	if err == nil {
		t.Fatal("expected error for empty token file")
	}
}

func TestVaultKeyProvider_TokenFileMissing(t *testing.T) {
	_, err := NewVaultKeyProvider(&config.VaultTransitConfig{
		Address:   "https://vault.example.com",
		TokenFile: "/nonexistent/path/token",
		KeyName:   "test-key",
		MountPath: "transit",
	})
	if err == nil {
		t.Fatal("expected error for missing token file")
	}
}

func TestVaultKeyProvider_TokenFileInsecurePermissions(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "world-readable-token")
	if err := os.WriteFile(tokenFile, []byte("secret-token"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := NewVaultKeyProvider(&config.VaultTransitConfig{
		Address:   "https://vault.example.com",
		TokenFile: tokenFile,
		KeyName:   "test-key",
		MountPath: "transit",
	})
	if err == nil {
		t.Fatal("expected error for world-readable token file")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("insecure permissions")) {
		t.Errorf("error should mention insecure permissions, got: %v", err)
	}
}

func TestVaultKeyProvider_TokenFileGroupReadable(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "group-readable-token")
	if err := os.WriteFile(tokenFile, []byte("secret-token"), 0o640); err != nil {
		t.Fatal(err)
	}

	_, err := NewVaultKeyProvider(&config.VaultTransitConfig{
		Address:   "https://vault.example.com",
		TokenFile: tokenFile,
		KeyName:   "test-key",
		MountPath: "transit",
	})
	if err == nil {
		t.Fatal("expected error for group-readable token file")
	}
}

func TestVaultKeyProvider_Close(t *testing.T) {
	ts := newFakeVault(t)
	p := newTestVaultProvider(t, ts)

	// Close should not panic and should be idempotent
	p.Close()
	p.Close()
}

func TestVaultKeyProvider_DefaultRenewInterval(t *testing.T) {
	ts := newFakeVault(t)
	p, err := NewVaultKeyProvider(&config.VaultTransitConfig{
		Address:   ts.URL,
		Token:     "test-token",
		KeyName:   "test-key",
		MountPath: "transit",
		// RenewInterval not set — should default to 5m
	})
	if err != nil {
		t.Fatalf("NewVaultKeyProvider: %v", err)
	}
	t.Cleanup(p.Close)

	if p.renewInterval != 5*time.Minute {
		t.Errorf("renewInterval = %v, want 5m", p.renewInterval)
	}
}
