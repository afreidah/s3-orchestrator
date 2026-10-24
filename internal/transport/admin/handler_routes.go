// -------------------------------------------------------------------------------
// Admin API - Route Table, Registration, and Auth Middleware
//
// Author: Alex Freidah
//
// One table describes the whole admin surface: method, pattern, handler, the
// types the route exchanges, and what authorizes it. Register builds the mux
// from it, so a route cannot be served without declaring its shape, and the
// generated API description reads the same source the server routes with.
//
// The guard is applied by the registration loop rather than per entry, so an
// endpoint cannot ship unauthenticated by forgetting the wrapper. What it
// enforces is declared here too: a data-plane route names the permissions its
// caller needs, which is why a route that forgets them is a visible gap in this
// table rather than an absent check inside a handler.
// -------------------------------------------------------------------------------

package admin

import (
	"net/http"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"
	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// Paths served by more than one entry, named so the table cannot drift
// between the methods that share a route.
const (
	pathBackendDrain    = "/admin/api/backends/{name}/drain"
	pathBackendCordon   = "/admin/api/backends/{name}/cordon"
	pathOverReplication = "/admin/api/over-replication"
	pathUnreadable      = "/admin/api/unreadable"
	pathLogLevel        = "/admin/api/log-level"
	pathObjects         = "/admin/api/objects"
	pathObject          = pathObjects + "/" + objectKeyPattern
	pathObjectTags      = pathObjects + "/tags/" + objectKeyPattern

	pathProvisioning = "/admin/api/provisioning"
	pathProvBuckets  = pathProvisioning + "/buckets"
	pathProvUsers    = pathProvisioning + "/users"
	pathProvCreds    = pathProvisioning + "/credentials"
	pathProvGrants   = pathProvisioning + "/grants"
)

// param is one query or path parameter a handler reads. Declaring them on the
// entry keeps the generated description complete: a caller cannot discover
// ?batch_size= by reading handler source.
type param struct {
	Name        string
	In          string
	Description string
	Required    bool
	Type        string
}

// Parameter names and descriptions shared by more than one entry, named so
// the same parameter cannot be described two ways on two routes.
const (
	paramName       = "name"
	paramID         = "id"
	paramKind       = "kind"
	paramBackend    = "backend"
	paramBatchSize  = "batch_size"
	paramMax        = "max"
	paramKey        = "key"
	paramPrefix     = "prefix"
	descBackendName = "Backend name"
	descObjectKey   = "Object key, including its bucket prefix"
	descBucketName  = "Virtual bucket name"
	descUserID      = "User ID"

	descResourceName = "Name of the granted resource, or * for every one of its kind"
	descResourceKind = "Kind of the granted resource: bucket, backend or orchestrator; defaults to bucket"

	descBulkRewriteMax = "Cap the objects rewritten by this request; 0 converts the whole fleet"
	descBackendScope   = "Restrict the pass to one backend"
)

// Parameter locations and types, mirrored by the generator.
const (
	inQuery     = "query"
	inPath      = "path"
	typeString  = "string"
	typeInteger = "integer"
	typeBoolean = "boolean"
)

// mediaOctetStream is the media type of every route that carries raw bytes
// rather than a JSON document.
const mediaOctetStream = "application/octet-stream"

// The parameters more than one entry declares, built once so a route cannot
// describe a shared parameter differently from its siblings.
var (
	objectKeyPath     = param{Name: paramKey, In: inPath, Required: true, Type: typeString, Description: descObjectKey}
	backendScope      = param{Name: paramBackend, In: inQuery, Type: typeString, Description: descBackendScope}
	bucketNamePath    = param{Name: paramName, In: inPath, Required: true, Type: typeString, Description: descBucketName}
	userIDPath        = param{Name: paramID, In: inPath, Required: true, Type: typeString, Description: descUserID}
	resourceNamePath  = param{Name: paramName, In: inPath, Required: true, Type: typeString, Description: descResourceName}
	resourceKindQuery = param{Name: paramKind, In: inQuery, Type: typeString, Description: descResourceKind}
)

// route is one admin endpoint. Request, Stream, Alt and ResponseType are zero
// for the routes that do not need them, which is most of them.
//
// Stream is the event emitted per line when a caller sends Accept:
// application/x-ndjson. Alt is a second success shape under the same status
// code (the backend-removal preview and its executed acknowledgement). The two
// media-type overrides are empty for JSON and set where a route carries raw
// bytes.
//
// Kind, Perm and Resource authorize a route: the kind of resource, the
// permissions the caller's grant must carry, and the parameter naming the
// resource. Kind is empty on a data-plane route, where Resource names the
// object key; on a backend route an absent Resource value means the whole
// fleet, which only a wildcard grant reaches. A route declaring no Perm
// authorizes nobody.
type route struct {
	Method       string
	Pattern      string
	Handler      http.HandlerFunc
	Summary      string
	Request      any
	Response     any
	Stream       any
	Alt          any
	Params       []param
	ResponseType string
	RequestType  string
	Kind         core.ResourceKind
	Perm         core.PermissionSet
	Resource     string
}

// kind reports what this route's permissions are over, defaulting to the bucket
// a data-plane route names.
func (rt *route) kind() core.ResourceKind {
	if rt.Kind == "" {
		return core.ResourceBucket
	}
	return rt.Kind
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// routes describes every admin endpoint. Adding an entry here is what mounts
// it -- there is no other registration path.
func (h *Handler) routes() []route {
	return []route{
		{
			Method: http.MethodGet, Pattern: "/admin/api/status", Handler: h.handleStatus,
			Summary:  "Instance and per-backend operational state",
			Response: adminapi.StatusResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminRead,
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/reload-status", Handler: h.handleReloadStatus,
			Summary:  "Outcome of the most recent config reload",
			Response: adminapi.ReloadStatusResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminRead,
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/config", Handler: h.handleConfig,
			Summary:  "Running configuration, with secrets redacted",
			Response: adminapi.ConfigResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminRead,
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/workers", Handler: h.handleWorkers,
			Summary:  "Last-tick health of every background service",
			Response: adminapi.WorkersResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminRead,
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/logs", Handler: LogsHandler(h.logs),
			Summary:  "Recent records from the in-memory log buffer",
			Response: adminapi.LogsResponse{},
			Params: []param{
				{Name: "level", In: inQuery, Type: typeString, Description: "Minimum level to return: debug, info, warn or error"},
				{Name: "since", In: inQuery, Type: typeString, Description: "RFC3339 timestamp; only records at or after it"},
				{Name: "before", In: inQuery, Type: typeString, Description: "RFC3339 timestamp; only records before it"},
				{Name: "component", In: inQuery, Type: typeString, Description: "Only records from this component"},
				{Name: "limit", In: inQuery, Type: typeInteger, Description: "Maximum records to return, newest kept (default 200, max 1000)"},
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminLogs,
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/object-locations", Handler: h.handleObjectLocations,
			Summary:  "Backend placement for one object key",
			Response: adminapi.ObjectLocationsResponse{},
			Params: []param{
				{Name: paramKey, In: inQuery, Required: true, Type: typeString, Description: "Object key to resolve"},
			},
			Perm: core.PermRead, Resource: paramKey,
		},
		{
			Method: http.MethodGet, Pattern: pathObjects, Handler: h.handleListObjects,
			Summary:  "Page of stored objects",
			Response: adminapi.ObjectListResponse{},
			Params: []param{
				{Name: paramPrefix, In: inQuery, Type: typeString, Description: "Restrict the listing to keys under this prefix"},
				{Name: "delimiter", In: inQuery, Type: typeString, Description: "Grouping delimiter; omitted defaults to /, empty lists every key flat"},
				{Name: "continuation", In: inQuery, Type: typeString, Description: "Continuation token from a previous page"},
			},
			Perm: core.PermList, Resource: paramPrefix,
		},
		{
			Method: http.MethodGet, Pattern: pathObject, Handler: h.handleGetObject,
			Summary:      "Download one object",
			ResponseType: mediaOctetStream,
			Params: []param{
				objectKeyPath,
			},
			Perm: core.PermRead, Resource: paramKey,
		},
		{
			Method: http.MethodPut, Pattern: pathObject, Handler: h.handlePutObject,
			Summary:     "Upload one object",
			RequestType: mediaOctetStream,
			Response:    adminapi.ObjectUploadResponse{},
			Params: []param{
				objectKeyPath,
			},
			Perm: core.PermWrite, Resource: paramKey,
		},
		{
			Method: http.MethodGet, Pattern: pathObjectTags, Handler: h.handleGetObjectTags,
			Summary:  "Read one object's tag set",
			Response: adminapi.ObjectTagsResponse{},
			Params: []param{
				objectKeyPath,
			},
			Perm: core.PermRead, Resource: paramKey,
		},
		{
			Method: http.MethodPut, Pattern: pathObjectTags, Handler: h.handlePutObjectTags,
			Summary:  "Replace one object's tag set",
			Request:  adminapi.ObjectTagsRequest{},
			Response: adminapi.ObjectTagsResponse{},
			Params: []param{
				objectKeyPath,
			},
			Perm: core.PermTags, Resource: paramKey,
		},
		{
			Method: http.MethodDelete, Pattern: pathObjectTags, Handler: h.handleDeleteObjectTags,
			Summary:  "Clear one object's tag set",
			Response: adminapi.ObjectTagsResponse{},
			Params: []param{
				objectKeyPath,
			},
			Perm: core.PermTags, Resource: paramKey,
		},
		{
			Method: http.MethodDelete, Pattern: pathObject, Handler: h.handleDeleteObject,
			Summary:  "Delete one object",
			Response: adminapi.ObjectDeleteResponse{},
			Params: []param{
				objectKeyPath,
			},
			Perm: core.PermDelete, Resource: paramKey,
		},
		{
			Method: http.MethodDelete, Pattern: pathObjects, Handler: h.handleDeletePrefix,
			Summary:  "Delete every object under a prefix",
			Response: adminapi.ObjectDeleteResponse{},
			Params: []param{
				{Name: paramPrefix, In: inQuery, Required: true, Type: typeString, Description: "Prefix whose objects are removed"},
			},
			Perm: core.PermDelete, Resource: paramPrefix,
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/cleanup-queue", Handler: h.handleCleanupQueue,
			Summary:  "Pending cleanup depth and a page of rows",
			Response: adminapi.CleanupQueueResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminRead,
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/cleanup-dlq", Handler: h.handleCleanupDLQ,
			Summary:  "Dead-lettered cleanup depth and a page of rows",
			Response: adminapi.CleanupDLQResponse{},
			Params: []param{
				{Name: paramBackend, In: inQuery, Type: typeString, Description: "Restrict the listing to one backend"},
				{Name: "limit", In: inQuery, Type: typeInteger, Description: "Maximum rows to return"},
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminRead,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/cleanup-dlq/requeue", Handler: h.handleCleanupDLQRequeue,
			Summary:  "Move dead-lettered cleanups back into the queue",
			Response: adminapi.CleanupDLQRequeueResponse{},
			Params: []param{
				{Name: paramBackend, In: inQuery, Type: typeString, Description: "Restrict the requeue to one backend"},
			},
			Kind: core.ResourceBackend, Perm: core.PermAdminMaintain, Resource: paramBackend,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/usage-flush", Handler: h.handleUsageFlush,
			Summary:  "Force a flush of usage counters to the database",
			Response: adminapi.UsageFlushResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminMaintain,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/usage-reconcile", Handler: h.handleReconcileUsage,
			Summary:  "Recompute per-backend bytes_used from the object ledger",
			Response: adminapi.UsageReconcileResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminMaintain,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/replicate", Handler: h.handleReplicate,
			Summary:  "Run one replication cycle",
			Response: adminapi.ReplicateResponse{},
			Stream:   adminstream.Event{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminMaintain,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/rebalance", Handler: h.handleRebalance,
			Summary:  "Run one rebalance cycle",
			Response: adminapi.RebalanceResponse{},
			Stream:   adminstream.Event{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminMaintain,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/lifecycle", Handler: h.handleLifecycle,
			Summary:  "Run one lifecycle expiration sweep",
			Response: adminapi.LifecycleResponse{},
			Stream:   adminstream.Event{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminMaintain,
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/replication", Handler: h.handleReplicationStatus,
			Summary:  "Replication backlog snapshot",
			Response: adminapi.ReplicationStatusResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminRead,
		},
		{
			Method: http.MethodGet, Pattern: pathOverReplication, Handler: h.handleOverReplicationStatus,
			Summary:  "Count of objects holding surplus copies",
			Response: adminapi.OverReplicationStatusResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminRead,
		},
		{
			Method: http.MethodPost, Pattern: pathOverReplication, Handler: h.handleOverReplicationClean,
			Summary:  "Run one over-replication cleanup pass",
			Response: adminapi.OverReplicationCleanResponse{},
			Params: []param{
				{Name: paramBatchSize, In: inQuery, Type: typeInteger, Description: "Override the configured batch size for this run"},
			},
			Stream: adminstream.Event{},
			Kind:   core.ResourceOrchestrator, Perm: core.PermAdminMaintain,
		},
		{
			Method: http.MethodGet, Pattern: pathLogLevel, Handler: h.handleLogLevel,
			Summary:  "Current runtime log level",
			Response: adminapi.LogLevelResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminRead,
		},
		{
			Method: http.MethodPut, Pattern: pathLogLevel, Handler: h.handleLogLevel,
			Summary:  "Set the runtime log level",
			Request:  adminapi.SetLogLevelRequest{},
			Response: adminapi.LogLevelResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminConfig,
		},
		{
			Method: http.MethodPost, Pattern: pathBackendDrain, Handler: h.handleStartDrain,
			Summary:  "Start draining a backend",
			Response: adminapi.BackendOperationResponse{},
			Params: []param{
				{Name: paramName, In: inPath, Required: true, Type: typeString, Description: descBackendName},
			},
			Kind: core.ResourceBackend, Perm: core.PermAdminDrain, Resource: paramName,
		},
		{
			Method: http.MethodGet, Pattern: pathBackendDrain, Handler: h.handleDrainProgress,
			Summary:  "Progress of an in-flight drain",
			Response: adminapi.DrainProgressResponse{},
			Params: []param{
				{Name: paramName, In: inPath, Required: true, Type: typeString, Description: descBackendName},
			},
			Kind: core.ResourceBackend, Perm: core.PermAdminRead, Resource: paramName,
		},
		{
			Method: http.MethodDelete, Pattern: pathBackendDrain, Handler: h.handleCancelDrain,
			Summary:  "Cancel an active drain",
			Response: adminapi.BackendOperationResponse{},
			Params: []param{
				{Name: paramName, In: inPath, Required: true, Type: typeString, Description: descBackendName},
			},
			Kind: core.ResourceBackend, Perm: core.PermAdminDrain, Resource: paramName,
		},
		{
			Method: http.MethodPost, Pattern: pathBackendCordon, Handler: h.handleCordon,
			Summary:  "Cordon a backend: stop new copies landing on it",
			Response: adminapi.BackendOperationResponse{},
			Params: []param{
				{Name: paramName, In: inPath, Required: true, Type: typeString, Description: descBackendName},
			},
			Kind: core.ResourceBackend, Perm: core.PermAdminCordon, Resource: paramName,
		},
		{
			Method: http.MethodDelete, Pattern: pathBackendCordon, Handler: h.handleUncordon,
			Summary:  "Uncordon a backend: let new copies land on it again",
			Response: adminapi.BackendOperationResponse{},
			Params: []param{
				{Name: paramName, In: inPath, Required: true, Type: typeString, Description: descBackendName},
			},
			Kind: core.ResourceBackend, Perm: core.PermAdminCordon, Resource: paramName,
		},
		{
			Method: http.MethodDelete, Pattern: "/admin/api/backends/{name}", Handler: h.handleRemoveBackend,
			Summary:  "Remove a backend, optionally purging its objects",
			Response: adminapi.BackendOperationResponse{},
			Params: []param{
				{Name: paramName, In: inPath, Required: true, Type: typeString, Description: descBackendName},
				{Name: "purge", In: inQuery, Type: typeBoolean, Description: "Also delete the backend's objects from its storage"},
				{Name: "confirm", In: inQuery, Type: typeString, Description: "Confirmation token from the preview call; required to execute a purge"},
			},
			Alt:    adminapi.RemoveBackendPreview{},
			Stream: adminstream.Event{},
			Kind:   core.ResourceBackend, Perm: core.PermAdminDecommission, Resource: paramName,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/rotate-encryption-key", Handler: h.handleRotateEncryptionKey,
			Summary:  "Re-wrap sealed DEKs under the current primary key",
			Request:  adminapi.RotateEncryptionKeyRequest{},
			Response: adminapi.RotateEncryptionKeyResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminKeys,
		},
		bulkRewriteRoute("/admin/api/encrypt-existing", h.handleEncryptExisting,
			"Encrypt every plaintext object on its own backend", adminapi.EncryptExistingResponse{}),
		bulkRewriteRoute("/admin/api/decrypt-existing", h.handleDecryptExisting,
			"Rewrite every encrypted object back to plaintext", adminapi.DecryptExistingResponse{}),
		bulkRewriteRoute("/admin/api/compress-existing", h.handleCompressExisting,
			"Store every uncompressed object as chunked zstd", adminapi.CompressExistingResponse{}),
		bulkRewriteRoute("/admin/api/decompress-existing", h.handleDecompressExisting,
			"Rewrite every compressed object back to its stored bytes", adminapi.DecompressExistingResponse{}),
		{
			Method: http.MethodPost, Pattern: "/admin/api/scrub", Handler: h.handleScrub,
			Summary:  "Verify stored content hashes against backend data",
			Response: adminapi.ScrubResponse{},
			Params: []param{
				backendScope,
				{Name: paramBatchSize, In: inQuery, Type: typeInteger, Description: "Objects verified in this pass"},
			},
			Stream: adminstream.Event{},
			Kind:   core.ResourceBackend, Perm: core.PermAdminMaintain, Resource: paramBackend,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/object-scrub", Handler: h.handleScrubKey,
			Summary:  "Verify every copy of one object now",
			Response: adminapi.ScrubKeyResponse{},
			Params: []param{
				{Name: paramKey, In: inQuery, Required: true, Type: typeString, Description: "Object key to verify"},
			},
			Perm: core.PermRead, Resource: paramKey,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/backfill-checksums", Handler: h.handleBackfillChecksums,
			Summary:  "Compute content hashes for objects missing one",
			Response: adminapi.BackfillChecksumsResponse{},
			Params: []param{
				backendScope,
				{Name: paramBatchSize, In: inQuery, Type: typeInteger, Description: "Objects hashed per pass"},
				{Name: paramMax, In: inQuery, Type: typeInteger, Description: "Cap the objects processed by this request; 0 drains the backlog"},
				{Name: "delay_ms", In: inQuery, Type: typeInteger, Description: "Pause between passes to rate-limit backend reads"},
			},
			Stream: adminstream.Event{},
			Kind:   core.ResourceBackend, Perm: core.PermAdminMaintain, Resource: paramBackend,
		},
		{
			Method: http.MethodGet, Pattern: pathUnreadable, Handler: h.handleListUnreadable,
			Summary:  "List copies that are encrypted with no key",
			Response: adminapi.UnreadableListResponse{},
			Params: []param{
				{Name: "limit", In: inQuery, Type: typeInteger, Description: "Maximum copies to return"},
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminRead,
		},
		{
			Method: http.MethodPost, Pattern: pathUnreadable, Handler: h.handlePurgeUnreadable,
			Summary:  "Delete every copy that is encrypted with no key",
			Response: adminapi.UnreadablePurgeResponse{},
			Params: []param{
				{Name: paramBatchSize, In: inQuery, Type: typeInteger, Description: "Copies purged per pass"},
			},
			Stream: adminstream.Event{},
			Kind:   core.ResourceOrchestrator, Perm: core.PermAdminMaintain,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/reconcile", Handler: h.handleReconcile,
			Summary:  "Reconcile backend storage against the object ledger",
			Response: adminapi.ReconcileResponse{},
			Params: []param{
				backendScope,
			},
			Stream: adminstream.Event{},
			Kind:   core.ResourceBackend, Perm: core.PermAdminMaintain, Resource: paramBackend,
		},
		{
			Method: http.MethodGet, Pattern: "/admin/api/cache", Handler: h.handleCacheStats,
			Summary:  "Object data cache utilization",
			Response: adminapi.CacheStatsResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminRead,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/cache/flush", Handler: h.handleCacheFlush,
			Summary:  "Drop every entry from the object data cache",
			Response: adminapi.CacheInvalidateResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminCache,
		},
		{
			Method: http.MethodDelete, Pattern: "/admin/api/cache/keys/{key...}", Handler: h.handleCacheInvalidateKey,
			Summary:  "Drop one key from the object data cache",
			Response: adminapi.CacheInvalidateKeyResponse{},
			Params: []param{
				{Name: paramKey, In: inPath, Required: true, Type: typeString, Description: "Full internal object key, slashes included"},
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminCache,
		},
		{
			Method: http.MethodDelete, Pattern: "/admin/api/cache/prefix", Handler: h.handleCacheInvalidatePrefix,
			Summary:  "Drop every cache entry under a key prefix",
			Response: adminapi.CacheInvalidateResponse{},
			Params: []param{
				{Name: paramPrefix, In: inQuery, Required: true, Type: typeString, Description: "Key prefix to invalidate"},
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminCache,
		},
		{
			Method: http.MethodPost, Pattern: "/admin/api/trace/snapshot", Handler: h.handleTraceSnapshot,
			Summary:      "Download a flight-recorder trace snapshot",
			ResponseType: mediaOctetStream,
			Kind:         core.ResourceOrchestrator, Perm: core.PermAdminLogs,
		},
		{
			Method: http.MethodGet, Pattern: pathProvisioning, Handler: h.handleProvisioning,
			Summary:  "Buckets, users and credentials from both the config file and the store",
			Response: adminapi.ProvisioningResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
		{
			Method: http.MethodPost, Pattern: pathProvBuckets, Handler: h.handleCreateBucket,
			Summary:  "Declare a virtual bucket",
			Request:  adminapi.CreateBucketRequest{},
			Response: adminapi.ProvisioningOperationResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
		{
			Method: http.MethodPatch, Pattern: pathProvBuckets + "/{name}", Handler: h.handleUpdateBucket,
			Summary:  "Replace what a virtual bucket carries",
			Request:  adminapi.UpdateBucketRequest{},
			Response: adminapi.ProvisioningOperationResponse{},
			Params: []param{
				bucketNamePath,
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
		{
			Method: http.MethodDelete, Pattern: pathProvBuckets + "/{name}", Handler: h.handleDeleteBucket,
			Summary:  "Remove a virtual bucket that holds no objects and no grants",
			Response: adminapi.ProvisioningOperationResponse{},
			Params: []param{
				bucketNamePath,
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
		{
			Method: http.MethodPost, Pattern: pathProvUsers, Handler: h.handleCreateUser,
			Summary:  "Declare an identity credentials can be issued against",
			Request:  adminapi.CreateUserRequest{},
			Response: adminapi.ProvisioningOperationResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
		{
			Method: http.MethodPatch, Pattern: pathProvUsers + "/{id}", Handler: h.handleRenameUser,
			Summary:  "Rename an identity, leaving the ID its credentials and grants reference",
			Request:  adminapi.RenameUserRequest{},
			Response: adminapi.ProvisioningOperationResponse{},
			Params: []param{
				userIDPath,
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
		{
			Method: http.MethodDelete, Pattern: pathProvUsers + "/{id}", Handler: h.handleDeleteUser,
			Summary:  "Remove an identity that holds no credentials and no grants",
			Response: adminapi.ProvisioningOperationResponse{},
			Params: []param{
				userIDPath,
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
		{
			Method: http.MethodPost, Pattern: pathProvCreds, Handler: h.handleCreateCredential,
			Summary:  "Register a keypair for a user, minting one when none is supplied",
			Request:  adminapi.CreateCredentialRequest{},
			Response: adminapi.CreateCredentialResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
		{
			Method: http.MethodDelete, Pattern: pathProvCreds + "/{id}", Handler: h.handleDeleteCredential,
			Summary:  "Revoke one keypair, leaving its siblings working",
			Response: adminapi.ProvisioningOperationResponse{},
			Params: []param{
				{Name: paramID, In: inPath, Required: true, Type: typeString, Description: "Access key ID to revoke"},
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
		{
			Method: http.MethodPost, Pattern: pathProvGrants, Handler: h.handleCreateGrant,
			Summary:  "Let a user reach a resource",
			Request:  adminapi.CreateGrantRequest{},
			Response: adminapi.ProvisioningOperationResponse{},
			Kind:     core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
		{
			Method: http.MethodPut, Pattern: pathProvGrants + "/{id}/{name}", Handler: h.handleSetGrant,
			Summary:  "Declare exactly what one user reaches on one resource",
			Request:  adminapi.SetGrantRequest{},
			Response: adminapi.ProvisioningOperationResponse{},
			Params: []param{
				userIDPath,
				resourceNamePath,
				resourceKindQuery,
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
		{
			Method: http.MethodDelete, Pattern: pathProvGrants + "/{id}/{name}", Handler: h.handleDeleteGrant,
			Summary:  "Withdraw one user's access to one resource",
			Response: adminapi.ProvisioningOperationResponse{},
			Params: []param{
				userIDPath,
				resourceNamePath,
				resourceKindQuery,
			},
			Kind: core.ResourceOrchestrator, Perm: core.PermAdminProvision,
		},
	}
}

// bulkRewriteRoute is one of the passes that rewrite every copy into another
// stored form on its own backend. They share every field but the path, the
// handler, the summary and the response type.
func bulkRewriteRoute(pattern string, handler http.HandlerFunc, summary string, response any) route {
	return route{
		Method: http.MethodPost, Pattern: pattern, Handler: handler,
		Summary:  summary,
		Response: response,
		Params: []param{
			backendScope,
			{Name: paramMax, In: inQuery, Type: typeInteger, Description: descBulkRewriteMax},
		},
		Stream: adminstream.Event{},
		Kind:   core.ResourceBackend, Perm: core.PermAdminConvert, Resource: paramBackend,
	}
}

// Register mounts the admin API routes on the given mux. Every entry is wrapped
// by the guard, so authentication and the route's declared permission are not
// something a handler can be written without.
func (h *Handler) Register(mux *http.ServeMux) {
	rts := h.routes()
	for i := range rts {
		mux.HandleFunc(rts[i].Method+" "+rts[i].Pattern, h.guard(&rts[i]))
	}
}
