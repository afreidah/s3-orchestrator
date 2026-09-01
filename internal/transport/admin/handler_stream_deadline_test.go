// -------------------------------------------------------------------------------
// Admin Handler - Streaming Write Deadline Tests
//
// Author: Alex Freidah
//
// A stream stays open for the whole pass, which outlasts server.write_timeout
// on any real fleet. These drive a live server whose timeout is shorter than
// the pass to hold that the deadline is lifted for streams and kept for
// everything else - the distinction the fix rests on.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// streamWriteTimeout is the server-side write deadline these tests run under.
// Short enough to keep the suite fast, and every pass below deliberately
// outlives it.
const streamWriteTimeout = 150 * time.Millisecond

// serveWithWriteTimeout starts a live server running h with the write deadline
// a long pass would otherwise trip. httptest.NewServer is used rather than a
// ResponseRecorder because a recorder has no deadline to trip: the bug only
// exists on a real net/http connection.
func serveWithWriteTimeout(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = streamWriteTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// get issues a GET against srv and returns the body, failing the test if the
// response cannot be read to completion.
func get(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

// TestStreamSteps_OutlivesTheServerWriteTimeout is the regression test for the
// TUI reporting INTERNAL_ERROR on a long scrub. The pass here runs to twice the
// write deadline; without the deadline being cleared the server kills the
// response mid-stream and the terminal result never arrives.
func TestStreamSteps_OutlivesTheServerWriteTimeout(t *testing.T) {
	t.Parallel()
	h := &Handler{}
	srv := serveWithWriteTimeout(t, func(w http.ResponseWriter, _ *http.Request) {
		h.streamSteps(w, "slow-op", "processing", true, func(obs progress.Observer) (stepResult, error) {
			// Two steps either side of the deadline: the first lands inside the
			// window, so a broken stream still looks healthy until the second.
			progress.Track(obs, "first", func() string { return progress.StatusOK })
			time.Sleep(2 * streamWriteTimeout)
			progress.Track(obs, "second", func() string { return progress.StatusOK })
			return stepResult{Processed: 2, Summary: "did the work"}, nil
		})
	})

	events := decodeEvents(t, get(t, srv))
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
}

// TestNonStreamingResponse_KeepsTheWriteTimeout holds the other half: the
// exemption is scoped to streams. A handler that clears the deadline for every
// response would quietly remove write_timeout from the whole admin API, which
// is a slow-client protection worth keeping.
func TestNonStreamingResponse_KeepsTheWriteTimeout(t *testing.T) {
	t.Parallel()
	srv := serveWithWriteTimeout(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * streamWriteTimeout)
		_, _ = w.Write([]byte("too late"))
	})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		if _, err = io.ReadAll(resp.Body); err == nil {
			t.Error("a response written past write_timeout should not have been delivered intact")
		}
	}
}
