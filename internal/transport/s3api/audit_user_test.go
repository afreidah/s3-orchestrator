// -------------------------------------------------------------------------------
// S3 API - Audit Identity Tests
//
// Author: Alex Freidah
//
// An audit entry has to name who took the action. These drive real requests
// through the server and read the emitted JSON, because the identity travels on
// the context rather than through a parameter: asserting on the transport's own
// struct would prove nothing about what the storage layer several packages
// deeper actually writes.
// -------------------------------------------------------------------------------

package s3api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// captureAudit installs a JSON handler for the duration of the test and returns
// every audit entry the body emitted, in order.
func captureAudit(t *testing.T, body func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	body()

	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["audit"] == true {
			out = append(out, entry)
		}
	}
	return out
}

// eventsNaming returns the audit entries whose event matches name.
func eventsNaming(entries []map[string]any, name string) []map[string]any {
	var out []map[string]any
	for _, e := range entries {
		if e["event"] == name {
			out = append(out, e)
		}
	}
	return out
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestAudit_PutCarriesTheUser is the property #1430 asked for: the entry names
// the identity behind the credential, so several services sharing a bucket with
// independent keys can be told apart after the fact.
func TestAudit_PutCarriesTheUser(t *testing.T) {
	ts, _, _ := newTestServer(t)

	entries := captureAudit(t, func() {
		resp := doReq(t, ts, http.MethodPut, ts.URL+"/mybucket/audited.txt", strings.NewReader("payload"))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})

	if len(entries) == 0 {
		t.Fatal("the request emitted no audit entries")
	}
	for _, e := range entries {
		if e["user"] == nil || e["user"] == "" {
			t.Errorf("entry %v carries no user", e["event"])
		}
	}
}

// TestAudit_BothCorrelatedEntriesCarryIt pins the reason the identity rides the
// context: one request writes an HTTP-layer entry and a storage-layer one from
// a package that is handed no identity, and both have to name the same caller
// under the same request id.
func TestAudit_BothCorrelatedEntriesCarryIt(t *testing.T) {
	ts, _, _ := newTestServer(t)

	entries := captureAudit(t, func() {
		resp := doReq(t, ts, http.MethodPut, ts.URL+"/mybucket/correlated.txt", strings.NewReader("payload"))
		defer resp.Body.Close()
	})

	http1 := eventsNaming(entries, "s3.PutObject")
	storage := eventsNaming(entries, "storage.PutObject")
	if len(http1) == 0 || len(storage) == 0 {
		t.Fatalf("want both layers to emit; got s3.PutObject=%d storage.PutObject=%d",
			len(http1), len(storage))
	}
	if http1[0]["user"] != storage[0]["user"] {
		t.Errorf("layers disagree on the user: %v vs %v", http1[0]["user"], storage[0]["user"])
	}
	if http1[0]["request_id"] != storage[0]["request_id"] {
		t.Errorf("layers disagree on the request id: %v vs %v",
			http1[0]["request_id"], storage[0]["request_id"])
	}
}

// TestAudit_RejectedRequestNamesNoUser verifies a request that failed to
// authenticate claims no identity. An empty or defaulted user on a rejection
// would attribute an action to someone who never proved they took it.
func TestAudit_RejectedRequestNamesNoUser(t *testing.T) {
	ts, _, _ := newTestServer(t)

	entries := captureAudit(t, func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/mybucket/x.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Proxy-Token", "wrong-token")
		resp, err := ts.Client().Do(req) //nolint:gosec // G704: test server URL
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})

	if len(entries) == 0 {
		t.Fatal("the rejection emitted no audit entry")
	}
	for _, e := range entries {
		if _, ok := e["user"]; ok {
			t.Errorf("rejection entry %v names user %v", e["event"], e["user"])
		}
	}
}

// TestAudit_BucketDeniedNamesTheUserOnce verifies the refusal names who was
// refused, and exactly once - the handler passed it explicitly before the
// context carried it, which would have emitted the key twice.
func TestAudit_BucketDeniedNamesTheUserOnce(t *testing.T) {
	ts, _, _ := newTestServer(t)

	entries := captureAudit(t, func() {
		resp := doReq(t, ts, http.MethodGet, ts.URL+"/other-bucket/x.txt", nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})

	denied := eventsNaming(entries, "s3.BucketDenied")
	if len(denied) != 1 {
		t.Fatalf("s3.BucketDenied entries = %d, want 1", len(denied))
	}
	if denied[0]["user"] == nil || denied[0]["user"] == "" {
		t.Error("the refusal does not name who was refused")
	}
	if denied[0]["requested_bucket"] != "other-bucket" {
		t.Errorf("requested_bucket = %v, want other-bucket", denied[0]["requested_bucket"])
	}
}
