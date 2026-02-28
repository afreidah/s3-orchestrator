// -------------------------------------------------------------------------------
// Backend Tests - S3 Client Configuration
//
// Author: Alex Freidah
//
// Unit tests for S3 backend client construction and option helpers.
// -------------------------------------------------------------------------------

package storage

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/afreidah/s3-orchestrator/internal/config"
)

func TestWithUnsignedPayload_AddsAPIOption(t *testing.T) {
	var opts s3.Options
	withUnsignedPayload(&opts)

	if len(opts.APIOptions) != 1 {
		t.Fatalf("expected 1 API option, got %d", len(opts.APIOptions))
	}
}

func TestNewS3Backend_UnsignedPayloadDefaults(t *testing.T) {
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
			unsignedPayload: boolPtr(false),
			wantUnsigned:    false,
		},
		{
			name:            "explicit true with http still forces signed",
			endpoint:        "http://example.com",
			unsignedPayload: boolPtr(true),
			wantUnsigned:    false,
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

func boolPtr(b bool) *bool { return &b }
