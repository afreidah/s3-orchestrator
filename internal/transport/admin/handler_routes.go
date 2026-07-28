// -------------------------------------------------------------------------------
// Admin API - Route Table, Registration, and Auth Middleware
//
// Author: Alex Freidah
//
// One table describes the whole admin surface: method, pattern, handler, and
// the types the route exchanges. Register builds the mux from it, so a route
// cannot be served without declaring its shape, and the generated API
// description reads the same source the server routes with. requireToken is
// applied by the registration loop rather than per entry, so an endpoint
// cannot ship unauthenticated by forgetting the wrapper; per-endpoint
// authorization (if any) lives inside the handler.
// -------------------------------------------------------------------------------

package admin

import (
	"crypto/subtle"
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// Paths served by more than one entry, named so the table cannot drift
// between the methods that share a route.
const (
	pathBackendDrain    = "/admin/api/backends/{name}/drain"
	pathOverReplication = "/admin/api/over-replication"
	pathLogLevel        = "/admin/api/log-level"
)

// route is one admin endpoint. Request, Stream, Alt and ResponseType are zero
// for the routes that do not need them, which is most of them.
type route struct {
	// Method and Pattern combine into the net/http mux pattern, kept apart
	// so the table stays greppable by path.
	Method  string
	Pattern string
	Handler http.HandlerFunc
	// Summary is the one-line description of the endpoint.
	Summary string
	// Request is the decoded request body; nil when the route takes none.
	Request any
	// Response is the success body. Nil only when ResponseType says the
	// route does not answer in JSON.
	Response any
	// Stream is the event emitted per line when the caller sends
	// Accept: application/x-ndjson. Nil for routes that only answer in JSON.
	Stream any
	// Alt is a second success shape the same route can return under the same
	// status code. Only two-phase backend removal needs one: the
	// confirmation preview and the executed acknowledgement share a route.
	Alt any
	// ResponseType overrides the success media type. Empty means JSON; the
	// trace snapshot serves a binary trace file and has no Response schema.
	ResponseType string
}

// routes describes every admin endpoint. Adding an entry here is what mounts
// it -- there is no other registration path.
func (h *Handler) routes() []route {
	return []route{
		{
			Method: http.MethodGet, Pattern: "/admin/api/status", Handler: h.handleStatus,
			Summary:  "Instance and per-backend operational state",
			Response: adminapi.StatusResponse{},
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/reload-status", Handler: h.handleReloadStatus,
			Summary:  "Outcome of the most recent config reload",
			Response: adminapi.ReloadStatusResponse{},
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/workers", Handler: h.handleWorkers,
			Summary:  "Last-tick health of every background service",
			Response: adminapi.WorkersResponse{},
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/logs", Handler: h.handleLogs,
			Summary:  "Recent records from the in-memory log buffer",
			Response: adminapi.LogsResponse{},
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/object-locations", Handler: h.handleObjectLocations,
			Summary:  "Backend placement for one object key",
			Response: adminapi.ObjectLocationsResponse{},
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/objects", Handler: h.handleListObjects,
			Summary:  "Page of stored objects",
			Response: adminapi.ObjectListResponse{},
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/cleanup-queue", Handler: h.handleCleanupQueue,
			Summary:  "Pending cleanup depth and a page of rows",
			Response: adminapi.CleanupQueueResponse{},
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/cleanup-dlq", Handler: h.handleCleanupDLQ,
			Summary:  "Dead-lettered cleanup depth and a page of rows",
			Response: adminapi.CleanupDLQResponse{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/cleanup-dlq/requeue", Handler: h.handleCleanupDLQRequeue,
			Summary:  "Move dead-lettered cleanups back into the queue",
			Response: adminapi.CleanupDLQRequeueResponse{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/usage-flush", Handler: h.handleUsageFlush,
			Summary:  "Force a flush of usage counters to the database",
			Response: adminapi.UsageFlushResponse{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/usage-reconcile", Handler: h.handleReconcileUsage,
			Summary:  "Recompute per-backend bytes_used from the object ledger",
			Response: adminapi.UsageReconcileResponse{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/replicate", Handler: h.handleReplicate,
			Summary:  "Run one replication cycle",
			Response: adminapi.ReplicateResponse{},
			Stream:   adminstream.Event{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/rebalance", Handler: h.handleRebalance,
			Summary:  "Run one rebalance cycle",
			Response: adminapi.RebalanceResponse{},
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/replication", Handler: h.handleReplicationStatus,
			Summary:  "Replication backlog snapshot",
			Response: adminapi.ReplicationStatusResponse{},
		},
		{
			Method: http.MethodGet, Pattern: pathOverReplication, Handler: h.handleOverReplicationStatus,
			Summary:  "Count of objects holding surplus copies",
			Response: adminapi.OverReplicationStatusResponse{},
		},
		{
			Method: http.MethodPost, Pattern: pathOverReplication, Handler: h.handleOverReplicationClean,
			Summary:  "Run one over-replication cleanup pass",
			Response: adminapi.OverReplicationCleanResponse{},
			Stream:   adminstream.Event{},
		},
		{
			Method: http.MethodGet, Pattern: pathLogLevel, Handler: h.handleLogLevel,
			Summary:  "Current runtime log level",
			Response: adminapi.LogLevelResponse{},
		},
		{
			Method: http.MethodPut, Pattern: pathLogLevel, Handler: h.handleLogLevel,
			Summary:  "Set the runtime log level",
			Request:  adminapi.SetLogLevelRequest{},
			Response: adminapi.LogLevelResponse{},
		},
		{
			Method: http.MethodPost, Pattern: pathBackendDrain, Handler: h.handleStartDrain,
			Summary:  "Start draining a backend",
			Response: adminapi.BackendOperationResponse{},
		},
		{
			Method: http.MethodGet, Pattern: pathBackendDrain, Handler: h.handleDrainProgress,
			Summary:  "Progress of an in-flight drain",
			Response: adminapi.DrainProgressResponse{},
		},
		{
			Method: http.MethodDelete, Pattern: pathBackendDrain, Handler: h.handleCancelDrain,
			Summary:  "Cancel an active drain",
			Response: adminapi.BackendOperationResponse{},
		},
		{
			Method: http.MethodDelete, Pattern: "/admin/api/backends/{name}", Handler: h.handleRemoveBackend,
			Summary:  "Remove a backend, optionally purging its objects",
			Response: adminapi.BackendOperationResponse{},
			Alt:      adminapi.RemoveBackendPreview{},
			Stream:   adminstream.Event{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/rotate-encryption-key", Handler: h.handleRotateEncryptionKey,
			Summary:  "Re-wrap sealed DEKs under the current primary key",
			Request:  adminapi.RotateEncryptionKeyRequest{},
			Response: adminapi.RotateEncryptionKeyResponse{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/encrypt-existing", Handler: h.handleEncryptExisting,
			Summary:  "Encrypt every plaintext object in place",
			Response: adminapi.EncryptExistingResponse{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/decrypt-existing", Handler: h.handleDecryptExisting,
			Summary:  "Rewrite every encrypted object back to plaintext",
			Response: adminapi.DecryptExistingResponse{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/scrub", Handler: h.handleScrub,
			Summary:  "Verify stored content hashes against backend data",
			Response: adminapi.ScrubResponse{},
			Stream:   adminstream.Event{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/backfill-checksums", Handler: h.handleBackfillChecksums,
			Summary:  "Compute content hashes for objects missing one",
			Response: adminapi.BackfillChecksumsResponse{},
			Stream:   adminstream.Event{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/reconcile", Handler: h.handleReconcile,
			Summary:  "Reconcile backend storage against the object ledger",
			Response: adminapi.ReconcileResponse{},
			Stream:   adminstream.Event{},
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/cache", Handler: h.handleCacheStats,
			Summary:  "Object data cache utilization",
			Response: adminapi.CacheStatsResponse{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/cache/flush", Handler: h.handleCacheFlush,
			Summary:  "Drop every entry from the object data cache",
			Response: adminapi.CacheInvalidateResponse{},
		},
		{
			Method: http.MethodDelete, Pattern: "/admin/api/cache/keys/{key...}", Handler: h.handleCacheInvalidateKey,
			Summary:  "Drop one key from the object data cache",
			Response: adminapi.CacheInvalidateKeyResponse{},
		},
		{
			Method: http.MethodDelete, Pattern: "/admin/api/cache/prefix", Handler: h.handleCacheInvalidatePrefix,
			Summary:  "Drop every cache entry under a key prefix",
			Response: adminapi.CacheInvalidateResponse{},
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/trace/snapshot", Handler: h.handleTraceSnapshot,
			Summary:      "Download a flight-recorder trace snapshot",
			ResponseType: "application/octet-stream",
		},
	}
}

// Register mounts the admin API routes on the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	rts := h.routes()
	for i := range rts {
		mux.HandleFunc(rts[i].Method+" "+rts[i].Pattern, h.requireToken(rts[i].Handler))
	}
}

// requireToken wraps a handler and enforces X-Admin-Token authentication.
func (h *Handler) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) != 1 {
			h.log.WarnContext(r.Context(), "unauthorized request", "path", r.URL.Path, "client_addr", r.RemoteAddr)
			httputil.WriteJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}
