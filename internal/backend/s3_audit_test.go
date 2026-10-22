// -------------------------------------------------------------------------------
// Backend Tests - Per-Request Audit Events
//
// Author: Alex Freidah
//
// Every request the orchestrator sends a backend writes one backend.<op> audit
// event naming the backend, the key, the bytes moved and the workload. These
// drive the S3 client against a fake endpoint and read the emitted entries.
// They capture the global logger, so none of them runs in parallel.
// -------------------------------------------------------------------------------

package backend

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit/audittest"
)

// onlyBackendEvent returns the single backend.<op> entry body emitted.
func onlyBackendEvent(t *testing.T, op string, body func()) audittest.Entry {
	t.Helper()
	got := audittest.Named(audittest.Capture(t, body), "backend."+op)
	if len(got) != 1 {
		t.Fatalf("backend.%s entries = %d, want 1", op, len(got))
	}
	return got[0]
}

// TestAudit_PutReportsBytesAndWorkload verifies a PUT names its backend, key,
// the bytes it sent and the workload on its context.
func TestAudit_PutReportsBytesAndWorkload(t *testing.T) {
	be := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"e"`)
	})
	ctx := audit.WithWorkload(t.Context(), audit.WorkloadReplication)

	e := onlyBackendEvent(t, "PutObject", func() {
		if _, err := be.PutObject(ctx, "dir/obj", bytes.NewReader(make([]byte, 42)), 42, "", nil); err != nil {
			t.Fatalf("PutObject: %v", err)
		}
	})
	for k, want := range map[string]any{"backend": "test", "key": "dir/obj", "bytes": 42.0, "workload": "replication"} {
		if e[k] != want {
			t.Errorf("%s = %v, want %v", k, e[k], want)
		}
	}
	if _, ok := e["error"]; ok {
		t.Errorf("successful request carries an error: %v", e["error"])
	}
}

// TestAudit_GetReportsWireBytes verifies a GET reports the bytes the backend
// sent, the same figure egress is charged.
func TestAudit_GetReportsWireBytes(t *testing.T) {
	payload := []byte("twelve bytes")
	be := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	})
	ctx := audit.WithWorkload(t.Context(), audit.WorkloadScrub)

	e := onlyBackendEvent(t, "GetObject", func() {
		res, err := be.GetObject(ctx, "obj", "")
		if err != nil {
			t.Fatalf("GetObject: %v", err)
		}
		_ = res.Body.Close()
	})
	if e["bytes"] != float64(len(payload)) || e["workload"] != "scrub" {
		t.Errorf("entry = %v, want %d bytes under scrub", e, len(payload))
	}
}

// TestAudit_FailedRequestReportsError verifies a failed request is still
// audited, with its error and no bytes moved.
func TestAudit_FailedRequestReportsError(t *testing.T) {
	be := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	e := onlyBackendEvent(t, "HeadObject", func() {
		if _, err := be.HeadObject(t.Context(), "obj"); err == nil {
			t.Fatal("HeadObject succeeded against a 403")
		}
	})
	if e["error"] == nil || e["bytes"] != 0.0 {
		t.Errorf("entry = %v, want an error and zero bytes", e)
	}
}

// TestAudit_ListAuditsEachPage verifies a listing writes one event per page it
// fetches, since each page is a request the backend charges.
func TestAudit_ListAuditsEachPage(t *testing.T) {
	be := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		truncated, next := "true", "<NextContinuationToken>t1</NextContinuationToken>"
		if r.URL.Query().Get("continuation-token") == "t1" {
			truncated, next = "false", ""
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<ListBucketResult><Name>test-bucket</Name><IsTruncated>%s</IsTruncated>%s`+
			`<Contents><Key>p/a</Key><Size>1</Size></Contents></ListBucketResult>`, truncated, next)
	})

	entries := audittest.Named(audittest.Capture(t, func() {
		if err := be.ListObjects(t.Context(), "p/", func([]ListedObject) error { return nil }); err != nil {
			t.Fatalf("ListObjects: %v", err)
		}
	}), "backend.ListObjectsV2")
	if len(entries) != 2 {
		t.Fatalf("list entries = %d, want one per page (2)", len(entries))
	}
	if entries[0]["key"] != "p/" {
		t.Errorf("key = %v, want the prefix", entries[0]["key"])
	}
}

// TestAudit_BatchDeleteReportsKeyCount verifies a batch request reports how
// many keys it carried rather than one of them.
func TestAudit_BatchDeleteReportsKeyCount(t *testing.T) {
	srv := &batchDeleteServer{}
	be := newTestBackend(t, srv.handle)

	e := onlyBackendEvent(t, "DeleteObjects", func() {
		if _, err := be.DeleteObjects(t.Context(), keys(3)); err != nil {
			t.Fatalf("DeleteObjects: %v", err)
		}
	})
	if e["keys"] != 3.0 {
		t.Errorf("keys = %v, want 3", e["keys"])
	}
	if _, ok := e["key"]; ok {
		t.Errorf("batch entry names a single key: %v", e["key"])
	}
}

// TestAudit_NoWorkloadLeavesFieldOut verifies a request made with no workload
// on its context still writes its event, without a workload field, so the
// integration check can find the path that forgot to set one.
func TestAudit_NoWorkloadLeavesFieldOut(t *testing.T) {
	be := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "")
	})

	e := onlyBackendEvent(t, "DeleteObject", func() {
		if err := be.DeleteObject(t.Context(), "obj"); err != nil {
			t.Fatalf("DeleteObject: %v", err)
		}
	})
	if _, ok := e["workload"]; ok {
		t.Errorf("workload = %v, want none", e["workload"])
	}
}
