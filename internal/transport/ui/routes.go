// -------------------------------------------------------------------------------
// UI Handler - Route Registration and Audit Table
//
// Author: Alex Freidah
//
// Single source of truth for every UI route. The uiAPIRoutes table pairs
// each route with the handler it dispatches to and a quotaTracking
// classification; the route-audit test reads the table to ensure every
// newly registered API route has been classified. Register iterates the
// table at startup to mount routes on the mux under the configured prefix.
// -------------------------------------------------------------------------------

package ui

import (
	"net/http"
)

// quotaTracking categorises a UI route's relationship to backend quota
// accounting. Tests in this package read the table to ensure every newly
// registered API route has been classified.
type quotaTracking int

// quotaTrackingNone and related constants used by this package.
const (
	// quotaTrackingNone is for routes that never reach a real backend
	// (HTML pages, JSON read-throughs, status pollers, log streamers).
	quotaTrackingNone quotaTracking = iota
	// quotaTrackingTracked is for routes whose backend op flows through
	// the same usage.Record / IncrementQuota / DecrementQuota calls used
	// by the S3-protocol handlers. The audit notes (uiAPIRoutes) cite
	// the exact recording site for each entry.
	quotaTrackingTracked
)

// uiAPIRoute pairs a UI route suffix with the handler it dispatches to and
// a tracking classification. The slice is the single source of truth for
// Register and for the route-audit test.
type uiAPIRoute struct {
	suffix   string
	handler  func(*Handler) http.HandlerFunc
	tracking quotaTracking
	// audit explains how the route's backend op (if any) reaches usage
	// tracking. Empty for quotaTrackingNone routes.
	audit string
}

// uiAPIRoutes is the full set of UI routes, with audit notes for any
// endpoint that may touch a real backend. Adding a new route to Register
// requires adding an entry here, which forces the developer to declare
// whether the route is quota-tracked.
var uiAPIRoutes = []uiAPIRoute{
	{"/api/dashboard", func(h *Handler) http.HandlerFunc { return h.handleAPIDashboard }, quotaTrackingNone, ""},
	{"/api/tree", func(h *Handler) http.HandlerFunc { return h.handleTreeAPI }, quotaTrackingNone, ""},
	{"/api/delete", func(h *Handler) http.HandlerFunc { return h.handleAPIDelete }, quotaTrackingTracked,
		"objects.DeleteObject -> objects_write.go usage.Record (1 API)"},
	{"/api/delete-prefix", func(h *Handler) http.HandlerFunc { return h.handleAPIDeletePrefix }, quotaTrackingTracked,
		"objects.ListObjects + DeleteObjects -> manager.go list pages + objects_write.go per-copy delete records"},
	{"/api/upload", func(h *Handler) http.HandlerFunc { return h.handleAPIUpload }, quotaTrackingTracked,
		"objects.PutObject -> objects_write.go usage.Record (1 API + ingress)"},
	{"/api/download", func(h *Handler) http.HandlerFunc { return h.handleAPIDownload }, quotaTrackingTracked,
		"objects.GetObject -> objects_read.go usage.Record (1 API + egress)"},
	{"/api/rebalance", func(h *Handler) http.HandlerFunc { return h.handleAPIRebalance }, quotaTrackingTracked,
		"rebalancer.Rebalance -> rebalancer.go Get+Delete egress and Put ingress records"},
	{"/api/rebalance/status", func(h *Handler) http.HandlerFunc { return h.handleAPIRebalanceStatus }, quotaTrackingNone, ""},
	{"/api/clean-excess", func(h *Handler) http.HandlerFunc { return h.handleAPICleanExcess }, quotaTrackingTracked,
		"overRep.Clean -> overreplication.go Delete API records"},
	{"/api/clean-excess/status", func(h *Handler) http.HandlerFunc { return h.handleAPICleanExcessStatus }, quotaTrackingNone, ""},
	{"/api/sync", func(h *Handler) http.HandlerFunc { return h.handleAPISync }, quotaTrackingTracked,
		"backendOps.SyncBackend -> manager.go list-page records"},
	{"/api/logs", func(h *Handler) http.HandlerFunc { return h.handleAPILogs }, quotaTrackingNone, ""},
	{"/api/replicate", func(h *Handler) http.HandlerFunc { return h.handleAPIReplicate }, quotaTrackingTracked,
		"adminHandler.Replicate -> replicator.go Get egress + Put ingress records"},
	{"/api/replicate/status", func(h *Handler) http.HandlerFunc { return h.handleAPIReplicateStatus }, quotaTrackingNone, ""},
	{"/api/scrub", func(h *Handler) http.HandlerFunc { return h.handleAPIScrub }, quotaTrackingTracked,
		"adminHandler.Scrub -> scrubber.readAndHash usage.Record (Get + egress)"},
	{"/api/scrub/status", func(h *Handler) http.HandlerFunc { return h.handleAPIScrubStatus }, quotaTrackingNone, ""},
	{"/api/backfill-checksums", func(h *Handler) http.HandlerFunc { return h.handleAPIBackfillChecksums }, quotaTrackingTracked,
		"adminHandler.BackfillChecksums -> scrubber.readAndHash usage.Record (Get + egress)"},
	{"/api/backfill-checksums/status", func(h *Handler) http.HandlerFunc { return h.handleAPIBackfillChecksumsStatus }, quotaTrackingNone, ""},
	{"/api/encrypt-existing", func(h *Handler) http.HandlerFunc { return h.handleAPIEncryptExisting }, quotaTrackingTracked,
		"adminHandler.EncryptExisting -> processBulkLocation backendOps.RecordUsage (Get + Put per object)"},
	{"/api/encrypt-existing/status", func(h *Handler) http.HandlerFunc { return h.handleAPIEncryptExistingStatus }, quotaTrackingNone, ""},
}

// Register mounts the UI routes on the given mux under the configured prefix.
func (h *Handler) Register(mux *http.ServeMux, prefix string) {
	h.prefix = prefix
	mux.HandleFunc(prefix+loginPath, h.handleLogin)
	mux.HandleFunc(prefix+"/logout", h.handleLogout)
	mux.HandleFunc(prefix+"/", h.requireAuth(h.handleDashboard))
	for _, route := range uiAPIRoutes {
		mux.HandleFunc(prefix+route.suffix, h.requireAuth(route.handler(h)))
	}
	mux.Handle(prefix+"/static/", http.StripPrefix(prefix+"/static/", http.FileServerFS(staticFS)))
}
