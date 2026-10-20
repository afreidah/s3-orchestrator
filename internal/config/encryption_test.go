// -------------------------------------------------------------------------------
// Encryption Config Tests - Key Loading
//
// Author: Alex Freidah
//
// Tests for the shared master key loaders, DecodeKey and ReadKeyFile, and the
// validation of every key source built on them, including previous_key_files
// and the restart-only treatment of a previous key change on reload.
// -------------------------------------------------------------------------------

package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeKey writes key to a file in a per-test directory and returns its path.
// Used for master_key_file and previous_key_files.
func writeKey(t *testing.T, key []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// validKey returns a master key of the right size with a recognizable fill.
// Each fill byte gives a different key.
func validKey(fill byte) []byte {
	return bytes.Repeat([]byte{fill}, KeySize)
}

// TestDecodeKey asserts a 32-byte base64 key decodes to its bytes and each
// unusable value fails with the sentinel naming why.
func TestDecodeKey(t *testing.T) {
	t.Parallel()
	key := validKey('k')
	got, err := DecodeKey(base64.StdEncoding.EncodeToString(key))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("DecodeKey(valid) = %x, %v; want the key", got, err)
	}

	for _, tc := range []struct {
		name string
		b64  string
		want error
	}{
		{"invalid base64", "not-valid-base64!!!", ErrInvalidBase64Key},
		{"short key", base64.StdEncoding.EncodeToString(make([]byte, 16)), ErrInvalidKeyLength},
		{"long key", base64.StdEncoding.EncodeToString(make([]byte, KeySize+1)), ErrInvalidKeyLength},
	} {
		if _, err := DecodeKey(tc.b64); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestReadKeyFile asserts a 32-byte key file reads back as its bytes and an
// unreadable or wrong-size file fails with the sentinel naming why.
func TestReadKeyFile(t *testing.T) {
	t.Parallel()
	key := validKey('f')
	got, err := ReadKeyFile(writeKey(t, key))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("ReadKeyFile(valid) = %x, %v; want the key", got, err)
	}

	if _, err := ReadKeyFile(filepath.Join(t.TempDir(), "absent.key")); !errors.Is(err, ErrInvalidKeyFile) {
		t.Errorf("missing file: err = %v, want ErrInvalidKeyFile", err)
	}
	if _, err := ReadKeyFile(writeKey(t, make([]byte, 16))); !errors.Is(err, ErrInvalidKeyLength) {
		t.Errorf("short file: err = %v, want ErrInvalidKeyLength", err)
	}
}

// TestEncryptionConfig_MasterKeyFile asserts a readable 32-byte key file
// passes validation and an unusable one fails naming the field.
func TestEncryptionConfig_MasterKeyFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		path string
		want error
	}{
		{"valid", writeKey(t, validKey('m')), nil},
		{"missing", filepath.Join(t.TempDir(), "absent.key"), ErrInvalidKeyFile},
		{"wrong size", writeKey(t, make([]byte, 16)), ErrInvalidKeyLength},
	} {
		cfg := validBaseConfig()
		cfg.Encryption = EncryptionConfig{Enabled: true, MasterKeyFile: tc.path}
		err := cfg.SetDefaultsAndValidate()
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "encryption.master_key_file") {
			t.Errorf("%s: err = %v, want %v naming encryption.master_key_file", tc.name, err, tc.want)
		}
	}
}

// TestEncryptionConfig_PreviousKeySources asserts every entry of
// previous_keys and previous_key_files is loaded, and an unusable entry fails
// as ErrPreviousKeyInvalid naming its field and index along with the reason.
func TestEncryptionConfig_PreviousKeySources(t *testing.T) {
	t.Parallel()
	masterKey := base64.StdEncoding.EncodeToString(validKey('p'))
	goodFile := writeKey(t, validKey('q'))
	goodB64 := base64.StdEncoding.EncodeToString(validKey('r'))

	for _, tc := range []struct {
		name      string
		keys      []string
		files     []string
		wantField string
		want      error
	}{
		{name: "valid", keys: []string{goodB64}, files: []string{goodFile}},
		{name: "bad previous key", keys: []string{goodB64, "not-valid!!!"}, wantField: "encryption.previous_keys[1]", want: ErrInvalidBase64Key},
		{name: "missing previous key file", files: []string{goodFile, filepath.Join(t.TempDir(), "absent.key")}, wantField: "encryption.previous_key_files[1]", want: ErrInvalidKeyFile},
		{name: "short previous key file", files: []string{writeKey(t, make([]byte, 8))}, wantField: "encryption.previous_key_files[0]", want: ErrInvalidKeyLength},
	} {
		cfg := validBaseConfig()
		cfg.Encryption = EncryptionConfig{Enabled: true, MasterKey: masterKey, PreviousKeys: tc.keys, PreviousKeyFiles: tc.files}
		err := cfg.SetDefaultsAndValidate()
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, ErrPreviousKeyInvalid) || !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.wantField) {
			t.Errorf("%s: err = %v, want ErrPreviousKeyInvalid and %v naming %s", tc.name, err, tc.want, tc.wantField)
		}
	}
}

// TestNonReloadableFieldsChanged_PreviousKeys asserts adding or changing a
// previous key, inline or as a file, is reported as a restart-only
// encryption change.
func TestNonReloadableFieldsChanged_PreviousKeys(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*EncryptionConfig){
		"previous_keys":      func(e *EncryptionConfig) { e.PreviousKeys = []string{"b2xk"} },
		"previous_key_files": func(e *EncryptionConfig) { e.PreviousKeyFiles = []string{"/etc/old.key"} },
	} {
		a := validBaseConfig()
		b := validBaseConfig()
		change(&b.Encryption)

		if changed := NonReloadableFieldsChanged(&a, &b); !slices.Equal(changed, []string{"encryption"}) {
			t.Errorf("%s: changed = %v, want [encryption]", name, changed)
		}
	}
}
