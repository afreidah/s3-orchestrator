// -------------------------------------------------------------------------------
// UI Admin Actions - Long-Running Operations Triggered from the Dashboard
//
// Author: Alex Freidah
//
// The dashboard runs the admin API's on-demand passes in the background: a
// trigger answers 202 Accepted at once and a status endpoint reports progress
// and, once the pass ends, the same adminapi response the admin API returns
// for it. Each action is an admin.Pass, so the dashboard and the API cannot
// disagree about how a pass ended or what it reports.
// -------------------------------------------------------------------------------

package ui

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// paramMax is the query parameter capping how many objects a bulk pass rewrites
// in one run, named to match the admin API's own.
const paramMax = "max"

// The states a status poll reports before a run has ended. Once it has, the
// status is how it ended: one of the adminstream outcomes.
const (
	statusStarted = "started"
	statusRunning = "running"
	statusIdle    = "idle"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// pass is a dashboard action: an admin.Pass with its result type erased,
// since the dashboard reports only the rendered response.
type pass interface {
	Name() string
	Run(ctx context.Context, log *slog.Logger) admin.Report
}

// action builds the pass a request triggers, or the error that makes the
// request malformed.
type action func(h *Handler, r *http.Request) (pass, error)

// actionStatus answers a status poll. Status is started, running or idle, or
// how the last run ended. Reason is a skip's reason and Error what a failure
// is called. Result is the pass's adminapi response when it ran or was skipped.
type actionStatus struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	Result any    `json:"result,omitempty"`
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// startAction refuses malformed parameters, ensures the action under key
// isn't already running, then runs p in a goroutine and answers 202 Accepted.
func (h *Handler) startAction(w http.ResponseWriter, key string, p pass, invalid error) {
	if invalid != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, invalid.Error())
		return
	}
	if !h.asyncOps.TryStart(key) {
		httputil.WriteJSONError(w, http.StatusConflict, p.Name()+" already running")
		return
	}

	go func() {
		ctx := audit.WithWorkload(context.Background(), audit.WorkloadAdmin)
		rep := p.Run(ctx, h.log)
		h.log.InfoContext(ctx, "dashboard action finished", "action", p.Name(), "outcome", rep.Result)
		h.asyncOps.Complete(key, &rep)
	}()

	httputil.WriteJSON(w, http.StatusAccepted, actionStatus{Status: statusStarted})
}

// writeActionStatus answers a status poll for the action under key.
func (h *Handler) writeActionStatus(w http.ResponseWriter, key string) {
	status := actionStatus{Status: statusIdle}
	rep, running := h.asyncOps.Status(key)
	switch {
	case running:
		status.Status = statusRunning
	case rep != nil:
		status = actionStatus{Status: rep.Result, Result: rep.Body}
		switch rep.Result {
		case adminstream.OutcomeSkipped:
			status.Reason = rep.Message
		case adminstream.OutcomeFailed:
			status.Error = rep.Message
		}
	}
	httputil.WriteJSON(w, http.StatusOK, status)
}

// -------------------------------------------------------------------------
// ACTIONS
// -------------------------------------------------------------------------

// rebalanceAction runs one rebalance cycle.
func rebalanceAction(h *Handler, _ *http.Request) (pass, error) {
	return admin.RebalancePass(h.d.Ops), nil
}

// cleanExcessAction removes surplus copies at the configured batch size.
func cleanExcessAction(h *Handler, _ *http.Request) (pass, error) {
	return admin.CleanExcessPass(h.d.Ops, 0), nil
}

// lifecycleAction runs one expiration sweep, so an operator can confirm a rule
// they just wrote matches something without waiting out the hourly tick.
func lifecycleAction(h *Handler, _ *http.Request) (pass, error) {
	return admin.LifecyclePass(h.d.Ops), nil
}

// replicateAction runs one replication cycle.
func replicateAction(h *Handler, _ *http.Request) (pass, error) {
	return admin.ReplicatePass(h.d.Ops), nil
}

// scrubAction runs one verification pass over every backend.
func scrubAction(h *Handler, _ *http.Request) (pass, error) {
	return admin.ScrubPass(h.d.Ops, 0, ""), nil
}

// backfillAction hashes every object that lacks a content hash.
func backfillAction(h *Handler, _ *http.Request) (pass, error) {
	return admin.BackfillPass(h.d.Ops, admin.BackfillParams{}), nil
}

// rewriteAction builds the bulk rewrite of kind. The request's max caps how
// many copies the run rewrites, 0 meaning the whole fleet.
func rewriteAction(kind admin.Rewrite) action {
	return func(h *Handler, r *http.Request) (pass, error) {
		maxObjects, err := httputil.QueryCount(r.URL.Query(), paramMax, 0, httputil.NoCeiling)
		return admin.RewritePass(h.d.Ops, kind, maxObjects, ""), err
	}
}
