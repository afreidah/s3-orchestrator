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
	"slices"

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
// tracked route's backend operation is charged through the same Acct()
// recorder and quota calls the S3-protocol handlers use, and the audit note on
// its table entry cites the recording site.
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
var uiAPIRoutes = slices.Concat(
	[]uiAPIRoute{
		{"/api/dashboard", func(h *Handler) http.HandlerFunc { return h.handleAPIDashboard }, quotaTrackingNone, "",
			onOrchestrator, adminRead},
		{"/api/tree", func(h *Handler) http.HandlerFunc { return h.handleTreeAPI }, quotaTrackingNone, "",
			onBucket, core.PermList},
		{"/api/delete", func(h *Handler) http.HandlerFunc { return h.handleAPIDelete }, quotaTrackingTracked,
			"object.DeleteObject -> cleanup Settler -> infra.deleteBatch Acct().APICall (DeleteObjects)",
			onBucket, core.PermDelete},
		{"/api/delete-prefix", func(h *Handler) http.HandlerFunc { return h.handleAPIDeletePrefix }, quotaTrackingTracked,
			"object.ListObjects (ledger only) + DeleteObject per key -> infra.deleteBatch Acct().APICall (DeleteObjects)",
			onBucket, core.PermDelete},
		{"/api/upload", func(h *Handler) http.HandlerFunc { return h.handleAPIUpload }, quotaTrackingTracked,
			"object.PutObject -> infra.Put Acct().Ingress (1 API + ingress)",
			onBucket, core.PermWrite},
		{"/api/download", func(h *Handler) http.HandlerFunc { return h.handleAPIDownload }, quotaTrackingTracked,
			"object.GetObject -> get.go meteredGet + Acct().Egress (1 API + egress)",
			onBucket, core.PermRead},
		{"/api/sync", func(h *Handler) http.HandlerFunc { return h.handleAPISync }, quotaTrackingTracked,
			"reconcile.SyncBackend -> pageBudget.charge Acct().APICall (ListObjects per page)",
			onBucket, core.PermWrite},
		{"/api/logs", func(h *Handler) http.HandlerFunc { return h.logs }, quotaTrackingNone, "",
			onOrchestrator, core.PermAdminLogs},
	},
	actionRoutes("/api/rebalance", (*Handler).rebalanceOp,
		"rebalancer.Rebalance -> infra.StreamCopy Acct().Egress + Ingress, source deletes via infra.deleteBatch",
		onOrchestrator, adminMaintain),
	actionRoutes("/api/clean-excess", (*Handler).cleanExcessOp,
		"overReplication.Clean -> infra.deleteBatch Acct().APICall (DeleteObjects)",
		onOrchestrator, adminMaintain),
	actionRoutes("/api/lifecycle", (*Handler).lifecycleOp,
		"expiry.ProcessRules -> object.DeleteObject -> infra.deleteBatch Acct().APICall (DeleteObjects)",
		onOrchestrator, adminMaintain),
	actionRoutes("/api/replicate", (*Handler).replicateOp,
		"ops.Replication.Replicate -> infra.StreamCopy Acct().Egress + Ingress",
		onOrchestrator, adminMaintain),
	actionRoutes("/api/scrub", (*Handler).scrubOp,
		"ops.Integrity.Scrub -> storedHasher.hashStored Acct().Egress (Get + egress)",
		onBackend, adminMaintain),
	actionRoutes("/api/backfill-checksums", (*Handler).backfillOp,
		"ops.Integrity.BackfillChecksums -> storedHasher.hashStored Acct().Egress (Get + egress)",
		onBackend, adminMaintain),
	actionRoutes("/api/encrypt-existing", (*Handler).encryptOp,
		"ops.Encryption.EncryptExisting -> bulkRewriteOp MoveObject -> infra.StreamCopy Acct().Egress + Ingress",
		onBackend, adminConvert),
	actionRoutes("/api/compress-existing", (*Handler).compressOp,
		"ops.Compression.CompressExisting -> bulkRewriteOp MoveObject -> infra.StreamCopy Acct().Egress + Ingress",
		onBackend, adminConvert),
	actionRoutes("/api/decompress-existing", (*Handler).decompressOp,
		"ops.Compression.DecompressExisting -> bulkRewriteOp MoveObject -> infra.StreamCopy Acct().Egress + Ingress",
		onBackend, adminConvert),
)

// actionRoutes is one async action's trigger and its status poll. The trigger
// starts the action in the background and answers 202; the poll only reads
// the tracker, so it needs what any dashboard read needs.
func actionRoutes[R any](
	suffix string,
	op func(*Handler, *http.Request) adminActionOp[R],
	audit string,
	kind core.ResourceKind,
	perm core.PermissionSet,
) []uiAPIRoute {
	trigger := func(h *Handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { h.startAdminAction(w, r, op(h, r)) }
	}
	status := func(h *Handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { h.writeAdminActionStatus(w, op(h, r)) }
	}
	return []uiAPIRoute{
		{suffix, trigger, quotaTrackingTracked, audit, kind, perm},
		{suffix + "/status", status, quotaTrackingNone, "", onOrchestrator, adminRead},
	}
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
