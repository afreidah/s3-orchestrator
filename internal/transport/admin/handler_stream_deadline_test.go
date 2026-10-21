// -------------------------------------------------------------------------------
// Admin Handler - Streaming Write Deadline Tests
//
// Author: Alex Freidah
//
// A stream stays open for the whole pass, which outlasts server.write_timeout
// on any real fleet. These drive a server whose timeout is shorter than the
// pass to hold that the deadline is lifted for streams and kept for everything
// else - the distinction the fix rests on. The server runs on the in-memory
// network inside a fake-clock bubble, so the timeout is a realistic one and
// costs no wall-clock time.
// -------------------------------------------------------------------------------

package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// streamWriteTimeout is the server-side write deadline these tests run under;
// every pass below deliberately outlives it.
const streamWriteTimeout = 30 * time.Second

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// getWithWriteTimeout serves h on a connection with the write deadline a long
// pass would otherwise trip, issues one GET, and returns the body and the
// error from reading it. A ResponseRecorder would not do: it has no deadline
// to trip, and the bug only exists on a net/http connection.
func getWithWriteTimeout(t *testing.T, h http.HandlerFunc) ([]byte, error) {
	t.Helper()
	srv := httptest.NewTestServer(t, h)
	srv.Config.WriteTimeout = streamWriteTimeout
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://admin/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestStreamSteps_OutlivesTheServerWriteTimeout is the regression test for the
// TUI reporting INTERNAL_ERROR on a long scrub. The pass here runs to twice the
// write deadline; without the deadline being cleared the server kills the
// response mid-stream and the terminal result never arrives.
func TestStreamSteps_OutlivesTheServerWriteTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &Handler{}
		body, err := getWithWriteTimeout(t, func(w http.ResponseWriter, _ *http.Request) {
			h.streamSteps(w, "slow-op", "processing", true, func(obs progress.Observer) (stepResult, error) {
				// Two steps either side of the deadline: the first lands inside
				// the window, so a broken stream still looks healthy until the
				// second.
				progress.Track(obs, "first", func() string { return progress.StatusOK })
				time.Sleep(2 * streamWriteTimeout)
				progress.Track(obs, "second", func() string { return progress.StatusOK })
				return stepResult{Processed: 2, Summary: "did the work"}, nil
			})
		})
		if err != nil {
			t.Fatalf("stream cut short: %v", err)
		}

		events := decodeEvents(t, body)
		if len(events) == 0 {
			t.Fatal("no events decoded; the stream was cut before anything arrived")
		}
		last := events[len(events)-1]
		if last.Kind != adminstream.KindResult {
			t.Fatalf("last event = %+v, want a %s event; the pass outlived the write deadline and was cut short",
				last, adminstream.KindResult)
		}
		if last.Outcome != adminstream.OutcomeOK || last.Processed != 2 {
			t.Errorf("result = %+v, want an ok outcome over 2 items", last)
		}
	})
}

// TestNonStreamingResponse_KeepsTheWriteTimeout holds the other half: the
// exemption is scoped to streams. A handler that clears the deadline for every
// response would quietly remove write_timeout from the whole admin API, which
// is a slow-client protection worth keeping.
func TestNonStreamingResponse_KeepsTheWriteTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body, err := getWithWriteTimeout(t, func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(2 * streamWriteTimeout)
			_, _ = w.Write([]byte("too late"))
		})
		if err == nil {
			t.Errorf("a response written past write_timeout was delivered intact: %q", body)
		}
	})
}
