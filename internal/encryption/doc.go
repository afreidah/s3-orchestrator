// Package encryption provides envelope encryption for object payloads.
// It frames plaintext into chunked AES-GCM segments addressable by byte
// range and supports pluggable key providers (config-embedded, file,
// and Vault transit).
package encryption
