// -------------------------------------------------------------------------------
// Admin CLI - object-tags Tests
//
// Author: Alex Freidah
//
// Covers the flag surface the command owns: the key=value parsing that lets an
// operator set tags without quoting JSON, the mutually exclusive modes, and
// the body the set mode sends.
// -------------------------------------------------------------------------------

package adminctl

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
)

// TestTagList_Set covers the key=value parsing.
func TestTagList_Set(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantKey string
		wantVal string
		wantErr bool
	}{
		{"simple pair", "a=1", "a", "1", false},
		{"empty value", "a=", "a", "", false},
		// Only the first separator splits, because a tag value is arbitrary
		// text and may itself contain "=".
		{"value containing a separator", "expr=a=b", "expr", "a=b", false},
		{"no separator", "novalue", "", "", true},
		{"empty key", "=1", "", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var tags tagList
			err := tags.Set(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Set(%q) succeeded, want an error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%q): %v", tc.in, err)
			}
			if len(tags) != 1 || tags[0].Key != tc.wantKey || tags[0].Value != tc.wantVal {
				t.Errorf("Set(%q) = %+v, want %s=%s", tc.in, tags, tc.wantKey, tc.wantVal)
			}
		})
	}
}

// TestTagList_String renders the collected pairs, which is what the flag
// package shows in usage output.
func TestTagList_String(t *testing.T) {
	t.Parallel()
	tags := tagList{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}}
	if got := tags.String(); got != "a=1,b=2" {
		t.Errorf("String() = %q, want %q", got, "a=1,b=2")
	}
}

// TestCmdObjectTags_RequiresKey verifies the command refuses without a key
// rather than requesting a path with an empty segment.
func TestCmdObjectTags_RequiresKey(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if Command("object-tags", nil, "http://unused", "tok", &stdout, &stderr) == 0 {
		t.Error("expected a non-zero exit without -key")
	}
	if !strings.Contains(stderr.String(), "-key is required") {
		t.Errorf("stderr = %q, want it to name the missing flag", stderr.String())
	}
}

// TestCmdObjectTags_ClearAndTagConflict verifies the two write modes are
// refused together: they describe different outcomes for the same call.
func TestCmdObjectTags_ClearAndTagConflict(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := Command("object-tags", []string{"-key", "bucket/k", "-clear", "-tag", "a=1"},
		"http://unused", "tok", &stdout, &stderr)
	if code == 0 {
		t.Error("expected a non-zero exit for -clear with -tag")
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("stderr = %q, want it to explain the conflict", stderr.String())
	}
}

// TestCmdObjectTags_SetSendsTheSet verifies the set mode PUTs the tags as a
// JSON body in the order given.
func TestCmdObjectTags_SetSendsTheSet(t *testing.T) {
	t.Parallel()
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tags":[]}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := Command("object-tags",
		[]string{"-key", "bucket/k", "-tag", "retain=30d", "-tag", "team=infra"},
		srv.URL, "tok", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}

	var sent adminapi.ObjectTagsRequest
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("unmarshal body: %v\n%s", err, gotBody)
	}
	if len(sent.Tags) != 2 || sent.Tags[0].Key != "retain" || sent.Tags[1].Key != "team" {
		t.Errorf("sent tags = %+v, want retain then team", sent.Tags)
	}
}
