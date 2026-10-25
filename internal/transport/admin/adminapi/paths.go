// -------------------------------------------------------------------------------
// Admin API - Paths
//
// Author: Alex Freidah
//
// Every admin API path, named once for the route table and the clients that
// call it. A fixed path is a constant; a path that carries an object key, a
// backend name or a provisioning identifier is built with Path, which escapes
// each one, so no caller can send one unescaped.
// -------------------------------------------------------------------------------

package adminapi

import (
	"net/url"
	"strings"
)

// Admin API paths. The ones a route extends with an identifier are the bases
// Path takes; the subpaths follow a backend's name.
const (
	PathStatus       = "/admin/api/status"
	PathLogs         = "/admin/api/logs"
	PathLogLevel     = "/admin/api/log-level"
	PathWorkers      = "/admin/api/workers"
	PathReloadStatus = "/admin/api/reload-status"
	PathConfig       = "/admin/api/config"
	PathTraceSnap    = "/admin/api/trace/snapshot"

	PathReplication     = "/admin/api/replication"
	PathReplicate       = "/admin/api/replicate"
	PathRebalance       = "/admin/api/rebalance"
	PathOverReplication = "/admin/api/over-replication"
	PathLifecycle       = "/admin/api/lifecycle"
	PathReconcile       = "/admin/api/reconcile"
	PathUsageFlush      = "/admin/api/usage-flush"
	PathUsageReconcile  = "/admin/api/usage-reconcile"

	PathScrub             = "/admin/api/scrub"
	PathObjectScrub       = "/admin/api/object-scrub"
	PathUnreadable        = "/admin/api/unreadable"
	PathBackfillChecksums = "/admin/api/backfill-checksums"

	PathEncryptExisting    = "/admin/api/encrypt-existing"
	PathDecryptExisting    = "/admin/api/decrypt-existing"
	PathCompressExisting   = "/admin/api/compress-existing"
	PathDecompressExisting = "/admin/api/decompress-existing"
	PathRotateKey          = "/admin/api/rotate-encryption-key"

	PathCache       = "/admin/api/cache"
	PathCacheFlush  = PathCache + "/flush"
	PathCacheKeys   = PathCache + "/keys"
	PathCachePrefix = PathCache + "/prefix"

	PathCleanupQueue      = "/admin/api/cleanup-queue"
	PathCleanupDLQ        = "/admin/api/cleanup-dlq"
	PathCleanupDLQRequeue = PathCleanupDLQ + "/requeue"

	PathObjects         = "/admin/api/objects"
	PathObjectTags      = PathObjects + "/tags"
	PathObjectLocations = "/admin/api/object-locations"

	PathBackends  = "/admin/api/backends"
	SubpathDrain  = "/drain"
	SubpathCordon = "/cordon"

	PathProvisioning = "/admin/api/provisioning"
	PathProvBuckets  = PathProvisioning + "/buckets"
	PathProvUsers    = PathProvisioning + "/users"
	PathProvCreds    = PathProvisioning + "/credentials"
	PathProvGrants   = PathProvisioning + "/grants"
)

// Path addresses one resource under base, appending each identifier as its
// own escaped segment. A key's slashes are escaped too, which the server's
// wildcard routes decode back into the key.
func Path(base string, ids ...string) string {
	var b strings.Builder
	b.WriteString(base)
	for _, id := range ids {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(id))
	}
	return b.String()
}
