// -------------------------------------------------------------------------------
// Backend Tests - S3 Client Configuration
//
// Author: Alex Freidah
//
// Unit tests for S3 backend client construction and option helpers.
// -------------------------------------------------------------------------------

package backend

import (
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestWithUnsignedPayload_AddsAPIOption(t *testing.T) {
	t.Parallel()
	var opts s3.Options
	withUnsignedPayload(&opts)

	if len(opts.APIOptions) != 1 {
		t.Fatalf("expected 1 API option, got %d", len(opts.APIOptions))
	}
}

func TestNewS3Backend_UnsignedPayloadDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		endpoint        string
		unsignedPayload *bool
		wantUnsigned    bool
	}{
		{
			name:         "https defaults to unsigned",
			endpoint:     "https://example.com",
			wantUnsigned: true,
		},
		{
			name:         "http forces signed",
			endpoint:     "http://example.com",
			wantUnsigned: false,
		},
		{
			name:            "explicit false overrides https",
			endpoint:        "https://example.com",
			unsignedPayload: func() *bool { b := false; return &b }(),
			wantUnsigned:    false,
		},
		{
			name:            "explicit true with http is respected",
			endpoint:        "http://example.com",
			unsignedPayload: func() *bool { b := true; return &b }(),
			wantUnsigned:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, err := NewS3Backend(&config.BackendConfig{
				Name:            "test",
				Endpoint:        tt.endpoint,
				Region:          "us-east-1",
				Bucket:          "test-bucket",
				AccessKeyID:     "AKID",
				SecretAccessKey: "secret",
				UnsignedPayload: tt.unsignedPayload,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if backend.unsignedPayload != tt.wantUnsigned {
				t.Errorf("unsignedPayload = %v, want %v", backend.unsignedPayload, tt.wantUnsigned)
			}
		})
	}
}

func TestNewS3Backend_DisableChecksum(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		disableChecksum bool
	}{
		{
			name:            "checksum disabled",
			disableChecksum: true,
		},
		{
			name:            "checksum enabled (default)",
			disableChecksum: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewS3Backend(&config.BackendConfig{
				Name:            "test",
				Endpoint:        "https://storage.googleapis.com",
				Region:          "us",
				Bucket:          "test-bucket",
				AccessKeyID:     "AKID",
				SecretAccessKey: "secret",
				DisableChecksum: tt.disableChecksum,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNewS3Backend_StripSDKHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		stripSDKHeaders bool
	}{
		{
			name:            "strip enabled",
			stripSDKHeaders: true,
		},
		{
			name:            "strip disabled (default)",
			stripSDKHeaders: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewS3Backend(&config.BackendConfig{
				Name:            "test",
				Endpoint:        "https://storage.googleapis.com",
				Region:          "auto",
				Bucket:          "test-bucket",
				AccessKeyID:     "AKID",
				SecretAccessKey: "secret",
				StripSDKHeaders: tt.stripSDKHeaders,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
