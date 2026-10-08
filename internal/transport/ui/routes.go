// -------------------------------------------------------------------------------
// UI Handler - Route Registration and Audit Table
//
// Author: Alex Freidah
//
// Single source of truth for every UI route. The uiAPIRoutes table pairs
// each route with the handler it dispatches to, a quotaTracking
// classification, and the permission it requires; the route-audit tests read
// the table to ensure every newly registered API route has been classified
// and authorized. Register iterates the table at startup to mount routes on
// the mux under the configured prefix.
// -------------------------------------------------------------------------------

package ui

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// quotaTracking categorises a UI route's relationship to backend quota
// accounting. Tests in this package read the table to ensure every newly
// registered API route has been classified.
type quotaTracking int

// quotaTrackingNone and quotaTrackingTracked classify what a route costs. A
// tracked route's backend operation flows through the same usage.Record,
// IncrementQuota and DecrementQuota calls the S3-protocol handlers use, and the
// audit note on its table entry cites the exact recording site.
const (
	quotaTrackingNone quotaTracking = iota // never reaches a backend: pages, read-throughs, pollers
	quotaTrackingTracked
)

// uiAPIRoute pairs a UI route suffix with the handler it dispatches to, a
// tracking classification, and what authorizes it. kind and perm mirror the
// admin API route for the same operation. A bucket route's handler authorizes
// the key it reads; every other kind is authorized from the table before the
// handler runs.
type uiAPIRoute struct {
	suffix   string
	handler  func(*Handler) http.HandlerFunc
	tracking quotaTracking
	audit    string // how the backend op reaches usage tracking; empty for untracked routes
	kind     core.ResourceKind
	perm     core.PermissionSet
}

// The kinds and permissions the table below repeats.
const (
	onOrchestrator = core.ResourceOrchestrator
	onBackend      = core.ResourceBackend
	onBucket       = core.ResourceBucket
	adminRead      = core.PermAdminRead
	adminMaintain  = core.PermAdminMaintain
	adminConvert   = core.PermAdminConvert
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// uiAPIRoutes is the full set of UI routes, with audit notes for any
// endpoint that may touch a real backend. Adding a new route to Register
// requires adding an entry here, which forces the developer to declare
// whether the route is quota-tracked.
var uiAPIRoutes = []uiAPIRoute{
	{"/api/dashboard", func(h *Handler) http.HandlerFunc { return h.handleAPIDashboard }, quotaTrackingNone, "",
		onOrchestrator, adminRead},
	{"/api/tree", func(h *Handler) http.HandlerFunc { return h.handleTreeAPI }, quotaTrackingNone, "",
		onBucket, core.PermList},
	{"/api/delete", func(h *Handler) http.HandlerFunc { return h.handleAPIDelete }, quotaTrackingTracked,
		"objects.DeleteObject -> objects_write.go usage.Record (1 API)",
		onBucket, core.PermDelete},
	{"/api/delete-prefix", func(h *Handler) http.HandlerFunc { return h.handleAPIDeletePrefix }, quotaTrackingTracked,
		"objects.ListObjects + DeleteObjects -> manager.go list pages + objects_write.go per-copy delete records",
		onBucket, core.PermDelete},
	{"/api/upload", func(h *Handler) http.HandlerFunc { return h.handleAPIUpload }, quotaTrackingTracked,
		"objects.PutObject -> objects_write.go usage.Record (1 API + ingress)",
		onBucket, core.PermWrite},
	{"/api/download", func(h *Handler) http.HandlerFunc { return h.handleAPIDownload }, quotaTrackingTracked,
		"objects.GetObject -> objects_read.go usage.Record (1 API + egress)",
		onBucket, core.PermRead},
	{"/api/rebalance", func(h *Handler) http.HandlerFunc { return h.handleAPIRebalance }, quotaTrackingTracked,
		"rebalancer.Rebalance -> rebalancer.go Get+Delete egress and Put ingress records",
		onOrchestrator, adminMaintain},
	{"/api/rebalance/status", func(h *Handler) http.HandlerFunc { return h.handleAPIRebalanceStatus }, quotaTrackingNone, "",
		onOrchestrator, adminRead},
	{"/api/clean-excess", func(h *Handler) http.HandlerFunc { return h.handleAPICleanExcess }, quotaTrackingTracked,
		"overRep.Clean -> overreplication.go Delete API records",
		onOrchestrator, adminMaintain},
	{"/api/clean-excess/status", func(h *Handler) http.HandlerFunc { return h.handleAPICleanExcessStatus }, quotaTrackingNone, "",
		onOrchestrator, adminRead},
	{"/api/lifecycle", func(h *Handler) http.HandlerFunc { return h.handleAPILifecycle }, quotaTrackingTracked,
		"expiry.ProcessRules -> objects.DeleteObject records one API call per expired copy",
		onOrchestrator, adminMaintain},
	{"/api/lifecycle/status", func(h *Handler) http.HandlerFunc { return h.handleAPILifecycleStatus }, quotaTrackingNone, "",
		onOrchestrator, adminRead},
	{"/api/sync", func(h *Handler) http.HandlerFunc { return h.handleAPISync }, quotaTrackingTracked,
		"backendOps.SyncBackend -> manager.go list-page records",
		onBucket, core.PermWrite},
	{"/api/logs", func(h *Handler) http.HandlerFunc { return h.handleAPILogs }, quotaTrackingNone, "",
		onOrchestrator, core.PermAdminLogs},
	{"/api/replicate", func(h *Handler) http.HandlerFunc { return h.handleAPIReplicate }, quotaTrackingTracked,
		"ops.Replication.Replicate -> replicator.go Get egress + Put ingress records",
		onOrchestrator, adminMaintain},
	{"/api/replicate/status", func(h *Handler) http.HandlerFunc { return h.handleAPIReplicateStatus }, quotaTrackingNone, "",
		onOrchestrator, adminRead},
	{"/api/scrub", func(h *Handler) http.HandlerFunc { return h.handleAPIScrub }, quotaTrackingTracked,
		"ops.Integrity.Scrub -> scrubber.readAndHash usage.Record (Get + egress)",
		onBackend, adminMaintain},
	{"/api/scrub/status", func(h *Handler) http.HandlerFunc { return h.handleAPIScrubStatus }, quotaTrackingNone, "",
		onOrchestrator, adminRead},
	{"/api/backfill-checksums", func(h *Handler) http.HandlerFunc { return h.handleAPIBackfillChecksums }, quotaTrackingTracked,
		"ops.Integrity.BackfillChecksums -> scrubber.readAndHash usage.Record (Get + egress)",
		onBackend, adminMaintain},
	{"/api/backfill-checksums/status", func(h *Handler) http.HandlerFunc { return h.handleAPIBackfillChecksumsStatus }, quotaTrackingNone, "",
		onOrchestrator, adminRead},
	{"/api/encrypt-existing", func(h *Handler) http.HandlerFunc { return h.handleAPIEncryptExisting }, quotaTrackingTracked,
		"ops.Encryption.EncryptExisting -> bulkRewriteOp.processLocation backendOps.RecordUsage (Get + Put per object)",
		onBackend, adminConvert},
	{"/api/encrypt-existing/status", func(h *Handler) http.HandlerFunc { return h.handleAPIEncryptExistingStatus }, quotaTrackingNone, "",
		onOrchestrator, adminRead},
	{"/api/compress-existing", func(h *Handler) http.HandlerFunc { return h.handleAPICompressExisting }, quotaTrackingTracked,
		"ops.Compression.CompressExisting -> bulkRewriteOp.processLocation backendOps.RecordUsage (Get + Put per object)",
		onBackend, adminConvert},
	{"/api/compress-existing/status", func(h *Handler) http.HandlerFunc { return h.handleAPICompressExistingStatus }, quotaTrackingNone, "",
		onOrchestrator, adminRead},
	{"/api/decompress-existing", func(h *Handler) http.HandlerFunc { return h.handleAPIDecompressExisting }, quotaTrackingTracked,
		"ops.Compression.DecompressExisting -> bulkRewriteOp.processLocation backendOps.RecordUsage (Get + Put per object)",
		onBackend, adminConvert},
	{"/api/decompress-existing/status", func(h *Handler) http.HandlerFunc { return h.handleAPIDecompressExistingStatus }, quotaTrackingNone, "",
		onOrchestrator, adminRead},
}

// overviewRoute is the overview page's authorization, the same one signing in
// requires.
var overviewRoute = uiAPIRoute{suffix: "/", kind: onOrchestrator, perm: adminRead}

// Register mounts the UI routes on the given mux under the configured prefix.
func (h *Handler) Register(mux *http.ServeMux, prefix string) {
	h.prefix = prefix
	mux.HandleFunc(prefix+loginPath, h.handleLogin)
	mux.HandleFunc(prefix+"/logout", h.handleLogout)
	mux.HandleFunc(prefix+"/", h.requireAuth(&overviewRoute, h.handleDashboard))
	for i := range uiAPIRoutes {
		route := &uiAPIRoutes[i]
		mux.HandleFunc(prefix+route.suffix, h.requireAuth(route, route.handler(h)))
	}
	mux.Handle(prefix+"/static/", http.StripPrefix(prefix+"/static/", http.FileServerFS(staticFS)))
}
