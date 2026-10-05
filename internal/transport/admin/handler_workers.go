// -------------------------------------------------------------------------------
// Admin API - Background Workers
//
// Author: Alex Freidah
//
// GET /admin/api/workers reports each background worker's last-tick health.
// POST /admin/api/workers/{name}/run runs one tick of a worker now, through
// the same advisory lock and health recording as a scheduled tick, so an
// operator watching a failing worker can retry it rather than wait for its
// interval. The worker reports nothing as it goes except its log lines, so a
// streaming caller is sent each record the tick logs, as it is logged,
// followed by the outcome.
// -------------------------------------------------------------------------------

package admin

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// Reasons reported when a worker run on request does not happen.
const (
	reasonWorkerDisabled = "the worker is not enabled on this deployment"
	reasonWorkerBusy     = "another instance is running this worker now"
)

// handleWorkers returns a snapshot of every registered background
// service's last-tick health. The supervisor records a tick outcome
// after every fire, so operators can identify stalled or repeatedly
// failing workers without scraping logs. Returns 503 when the
// lifecycle manager was not wired (proxy-only deployments that disable
// the worker pool).
func (h *Handler) handleWorkers(w http.ResponseWriter, _ *http.Request) {
	if h.workerHealth == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "worker health not available")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.WorkersResponse{Workers: h.workerHealth()})
}

// handleRunWorker runs one tick of the named worker. A name the worker list
// does not show is a 404, checked before anything starts, so a typo never
// opens a stream. Streams the tick's log lines and the outcome when the
// client accepts the stream content type; otherwise returns one JSON result.
func (h *Handler) handleRunWorker(w http.ResponseWriter, r *http.Request) {
	if h.runWorker == nil || h.workerHealth == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "workers not available")
		return
	}
	name := r.PathValue(paramName)
	if !h.workerKnown(name) {
		httputil.WriteJSONError(w, http.StatusNotFound, fmt.Sprintf("no worker named %q", name))
		return
	}
	if acceptsStream(r) {
		h.streamRunWorker(w, r, name)
		return
	}

	err := h.runWorker(r.Context(), name)
	if reason, skipped := workerSkipReason(err); skipped {
		httputil.WriteJSON(w, http.StatusOK, adminapi.WorkerRunResponse{Worker: name, Status: statusSkipped, Reason: reason})
		return
	}
	if err != nil {
		h.internalError(r.Context(), w, "worker run failed", err, "worker", name)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.WorkerRunResponse{Worker: name, Status: statusOK})
}

// workerKnown reports whether name is a worker the worker list shows.
func (h *Handler) workerKnown(name string) bool {
	return slices.ContainsFunc(h.workerHealth(), func(w adminapi.WorkerHealth) bool { return w.Name == name })
}

// streamRunWorker runs the tick with a log tap on its context, sending each
// record as a progress event, then the outcome. The tap closes before the
// result is sent, so a record logged by something the tick left running
// cannot land after it.
func (h *Handler) streamRunWorker(w http.ResponseWriter, r *http.Request, name string) {
	emit := newEventStream(w)
	emit(adminstream.Event{Kind: adminstream.KindStart, Op: "run " + name})

	var mu sync.Mutex
	open := true
	ctx := telemetry.WithLogTap(r.Context(), func(e telemetry.LogEntry) {
		mu.Lock()
		defer mu.Unlock()
		if open {
			emit(adminstream.Event{Kind: adminstream.KindProgress, Message: workerLogLine(&e)})
		}
	})

	start := time.Now()
	err := h.runWorker(ctx, name)
	mu.Lock()
	open = false
	mu.Unlock()
	emit(workerRunResult(err, time.Since(start)))
}

// workerRunResult is the closing event for a run: ok, skipped with the reason
// it did not run, or failed with the tick's error.
func workerRunResult(err error, elapsed time.Duration) adminstream.Event {
	ev := adminstream.Event{Kind: adminstream.KindResult, DurationMs: elapsed.Milliseconds()}
	if reason, skipped := workerSkipReason(err); skipped {
		ev.Outcome, ev.Message = adminstream.OutcomeSkipped, reason
		return ev
	}
	if err != nil {
		ev.Outcome, ev.Error = adminstream.OutcomeFailed, err.Error()
		return ev
	}
	ev.Outcome = adminstream.OutcomeOK
	return ev
}

// workerSkipReason reports why a run did not happen, when it did not.
func workerSkipReason(err error) (string, bool) {
	switch {
	case errors.Is(err, lifecycle.ErrWorkerDisabled):
		return reasonWorkerDisabled, true
	case errors.Is(err, lifecycle.ErrWorkerBusy):
		return reasonWorkerBusy, true
	}
	return "", false
}

// workerLogLine renders one log record as a progress line: the message, its
// attributes as sorted key=value pairs, and the level ahead of it when it is
// a warning or worse. The component is left out, since every line in the
// stream comes from the worker being run.
func workerLogLine(e *telemetry.LogEntry) string {
	var b strings.Builder
	if level := strings.ToUpper(e.Level); level == "WARN" || level == "ERROR" {
		b.WriteString(level + " ")
	}
	b.WriteString(e.Message)
	for _, k := range slices.Sorted(maps.Keys(e.Attrs)) {
		if k == "component" {
			continue
		}
		fmt.Fprintf(&b, " %s=%v", k, e.Attrs[k])
	}
	return b.String()
}
