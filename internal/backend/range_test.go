// -------------------------------------------------------------------------------
// Backend Tests - Range Emulation
//
// Author: Alex Freidah
//
// Drives S3Backend.GetObject against an httptest endpoint that ignores Range,
// and one that honours it, to pin the cut the backend makes on a whole-object
// reply, the 416 an unsatisfiable range becomes, and the wire bytes charged.
// -------------------------------------------------------------------------------

package backend

import (
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// rangePayload is the object every test in this file serves.
const rangePayload = "0123456789abcdefghij"

// ignoresRange answers every GET with 200 and the whole of rangePayload, as a
// backend without Range support does.
func ignoresRange(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Length", strconv.Itoa(len(rangePayload)))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, rangePayload)
}

// TestGetObject_CutsRangeFromWholeObject pins the body, size, Content-Range
// and wire bytes for each range form when the backend ignores Range, and that
// each occurrence is counted.
func TestGetObject_CutsRangeFromWholeObject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		header       string
		body         string
		contentRange string
		wire         int64
	}{
		{name: "explicit", header: "bytes=5-9", body: "56789", contentRange: "bytes 5-9/20", wire: 10},
		{name: "open ended", header: "bytes=15-", body: "fghij", contentRange: "bytes 15-19/20", wire: 20},
		{name: "suffix", header: "bytes=-3", body: "hij", contentRange: "bytes 17-19/20", wire: 20},
		{name: "end clamped", header: "bytes=18-99", body: "ij", contentRange: "bytes 18-19/20", wire: 20},
		{name: "from start", header: "bytes=0-1", body: "01", contentRange: "bytes 0-1/20", wire: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			be := newTestBackend(t, ignoresRange)
			before := testutil.ToFloat64(telemetry.BackendRangeIgnoredTotal.WithLabelValues(be.name))

			res, err := be.GetObject(t.Context(), "obj", tt.header)
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}
			assertRangeResult(t, res, tt.body, tt.contentRange, tt.wire)
			if after := testutil.ToFloat64(telemetry.BackendRangeIgnoredTotal.WithLabelValues(be.name)); after <= before {
				t.Errorf("range-ignored counter did not move: before %v, after %v", before, after)
			}
		})
	}
}

// assertRangeResult reads and closes res, requiring the body, size,
// Content-Range and wire bytes of the range it should hold.
func assertRangeResult(t *testing.T, res *GetObjectResult, body, contentRange string, wire int64) {
	t.Helper()
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil || string(got) != body {
		t.Errorf("body = %q, %v; want %q", got, err, body)
	}
	if res.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", res.Size, len(body))
	}
	if res.ContentRange != contentRange {
		t.Errorf("content range = %q, want %q", res.ContentRange, contentRange)
	}
	if res.WireBytes() != wire {
		t.Errorf("wire bytes = %d, want %d", res.WireBytes(), wire)
	}
}

// TestGetObject_IgnoredRangeUnsatisfiable reports a range past the end of the
// object as the same 416 a backend that honours Range returns.
func TestGetObject_IgnoredRangeUnsatisfiable(t *testing.T) {
	t.Parallel()
	be := newTestBackend(t, ignoresRange)

	_, err := be.GetObject(t.Context(), "obj", "bytes=20-29")
	if !isRangeNotSatisfiable(err) {
		t.Fatalf("err = %v, want a 416", err)
	}
	if got := classifyFailure(err); got != scopeNone {
		t.Errorf("breaker scope = %v, want none", got)
	}
}

// TestGetObject_IgnoredRangeShortBody fails a whole-object reply that ends
// before the range starts, instead of serving bytes from the wrong offset.
func TestGetObject_IgnoredRangeShortBody(t *testing.T) {
	t.Parallel()
	be := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, rangePayload)
	})

	if _, err := be.GetObject(t.Context(), "obj", "bytes=50-59"); err == nil {
		t.Fatal("GetObject succeeded on a body shorter than the range start")
	}
}

// TestGetObject_HonouredRangeUntouched leaves a 206 reply as the backend sent
// it, with nothing beyond its body charged.
func TestGetObject_HonouredRangeUntouched(t *testing.T) {
	t.Parallel()
	be := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=5-9" {
			t.Errorf("Range = %q, want bytes=5-9", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Length", "5")
		w.Header().Set("Content-Range", "bytes 5-9/20")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, rangePayload[5:10])
	})

	res, err := be.GetObject(t.Context(), "obj", "bytes=5-9")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	assertRangeResult(t, res, "56789", "bytes 5-9/20", 5)
}

// TestFetchEnvelopeHeader_IgnoredRange reads the header prefix of an object
// whose backend ignores Range, and treats an empty object as having none.
func TestFetchEnvelopeHeader_IgnoredRange(t *testing.T) {
	t.Parallel()
	be := newTestBackend(t, ignoresRange)
	hdr, err := FetchEnvelopeHeader(t.Context(), be, "obj")
	if err != nil || string(hdr) != rangePayload {
		t.Errorf("header = %q, %v; want the %d-byte object", hdr, err, len(rangePayload))
	}

	empty := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	})
	hdr, err = FetchEnvelopeHeader(t.Context(), empty, "obj")
	if err != nil || len(hdr) != 0 {
		t.Errorf("empty object header = %q, %v; want none", hdr, err)
	}
}
