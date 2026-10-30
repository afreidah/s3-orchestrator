// -------------------------------------------------------------------------------
// Admin API - Views Gathered From Every Instance
//
// Author: Alex Freidah
//
// Behind a load balancer each admin request reaches one instance, but some of
// what an operator asks about differs between instances: the log level each
// runs at, its last config reload, the workers it has ticked. Every instance
// publishes a report of its own on a heartbeat, and these endpoints answer
// from all of them, so the answer does not depend on which instance took the
// request.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// instanceRosterName is the shared-state name instance reports are filed under.
const instanceRosterName = "instance"

// instanceReportInterval is how often each instance republishes its report,
// which bounds how stale another instance's view of it can be.
const instanceReportInterval = 15 * time.Second

// -------------------------------------------------------------------------
// ENDPOINTS
// -------------------------------------------------------------------------

// handleInstances returns the report of every running instance.
func (h *Handler) handleInstances(w http.ResponseWriter, r *http.Request) {
	httputil.WriteJSON(w, http.StatusOK, adminapi.InstancesResponse{Instances: h.listInstances(r.Context())})
}

// handleWorkers returns each background service's last tick across the running
// instances, or 503 when no lifecycle manager is wired here.
func (h *Handler) handleWorkers(w http.ResponseWriter, r *http.Request) {
	if h.d.WorkerHealth == nil {
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "worker health not available")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, adminapi.WorkersResponse{Workers: mergeWorkerHealth(h.listInstances(r.Context()))})
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// instanceRoster builds the roster this instance publishes its report to and
// lists every instance's report from.
func (h *Handler) instanceRoster(s *sharedstate.Shared) *sharedstate.Roster[adminapi.InstanceReport] {
	return sharedstate.NewRoster(s, sharedstate.RosterConfig[adminapi.InstanceReport]{
		Name:     instanceRosterName,
		Interval: instanceReportInterval,
		Report:   h.instanceReport,
	})
}

// instanceReport is this instance's report as it stands now.
func (h *Handler) instanceReport(context.Context) adminapi.InstanceReport {
	rep := adminapi.InstanceReport{
		Instance:   h.d.Instance,
		ReportedAt: time.Now().UTC(),
		LogLevel:   h.levelName(),
		Reload:     h.reloadState(),
	}
	if h.d.WorkerHealth != nil {
		rep.Workers = h.d.WorkerHealth()
	}
	return rep
}

// listInstances lists every instance's report. A failed read is logged and
// answered with what was readable, which always includes this instance's own.
func (h *Handler) listInstances(ctx context.Context) []adminapi.InstanceReport {
	reports, err := h.instances.List(ctx)
	if err != nil {
		h.log.WarnContext(ctx, "instance reports not fully read", logfmt.Err(err))
	}
	return reports
}

// mergeWorkerHealth takes, for each worker any instance reports, the tick of
// the instance that ran it most recently, and names that instance. Workers are
// listed in the order they first appear, so the answering instance's own order
// leads.
func mergeWorkerHealth(reports []adminapi.InstanceReport) []adminapi.WorkerHealth {
	var merged []adminapi.WorkerHealth
	index := make(map[string]int)
	for i := range reports {
		for _, wh := range reports[i].Workers {
			if !lastTick(&wh).IsZero() {
				wh.Instance = reports[i].Instance.ID
			}
			at, seen := index[wh.Name]
			switch {
			case !seen:
				index[wh.Name] = len(merged)
				merged = append(merged, wh)
			case lastTick(&wh).After(lastTick(&merged[at])):
				merged[at] = wh
			}
		}
	}
	return merged
}

// lastTick is when a worker last ran to an outcome, success or failure.
func lastTick(wh *adminapi.WorkerHealth) time.Time {
	if wh.LastFailure.After(wh.LastSuccess) {
		return wh.LastFailure
	}
	return wh.LastSuccess
}
