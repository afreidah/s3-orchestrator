// -------------------------------------------------------------------------------
// Admin API Handler - Operational Control Endpoints
//
// Author: Alex Freidah
//
// HTTP handler for administrative operations exposed under /admin/api/. Provides
// endpoints for checking system status, inspecting object locations, managing the
// cleanup queue, flushing usage counters, triggering replication, and controlling
// the runtime log level. All endpoints require a valid X-Admin-Token header.
// -------------------------------------------------------------------------------

// Package admin provides the admin API handler for operational control endpoints.
package admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/proxy/dashboard"
	"github.com/afreidah/s3-orchestrator/internal/proxy/drain"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// BackendOps is the narrow surface of *proxy.BackendManager that the admin
// handler depends on for operations not encapsulated by a named sub-manager
// (replicator, drain, scrubber, etc.). *proxy.BackendManager satisfies it.
type BackendOps interface {
	GetDashboardData(ctx context.Context) (*dashboard.Data, error)
	FlushUsage(ctx context.Context) error
	UpdateQuotaMetrics(ctx context.Context) error
	RecordUsage(backendName string, requests, ingressBytes, egressBytes int64)
	GetBackend(name string) (backend.ObjectBackend, error)
	IntegrityConfig() *config.IntegrityConfig
}

// Compile-time assertion: *proxy.BackendManager implements BackendOps.
var _ BackendOps = (*proxy.BackendManager)(nil)

// Handler serves the admin API endpoints.
type Handler struct {
	log               *slog.Logger
	backendOps        BackendOps
	replicator        ReplicatorOps
	overRep           OverReplicationOps
	drain             *drain.Manager
	scrubber          ScrubberOps
	lifecycle         core.BackendLifecycleStore
	reconciler        Reconciler
	multipartBackfill MultipartBackfiller
	dbHealthy         func() bool
	objects           core.ObjectStore
	cleanup           core.CleanupStore
	encAdmin          core.EncryptionAdmin
	encryptor         *encryption.Encryptor
	token             string
	logLevel          *slog.LevelVar
}

// Deps groups the narrow role interfaces and infrastructure the admin
// handler touches. Each field carries the smallest contract the handler
// actually uses, so the constructor (and the backing DI provider) never
// hand the handler a god-shaped *proxy.BackendManager.
type Deps struct {
	BackendOps        BackendOps
	Replicator        ReplicatorOps
	OverRep           OverReplicationOps
	Drain             *drain.Manager
	Scrubber          ScrubberOps
	Lifecycle         core.BackendLifecycleStore
	DBHealthy         func() bool // typically *breaker.CircuitBreaker.IsHealthy
	Encryption        core.EncryptionAdmin
	Objects           core.ObjectStore
	Cleanup           core.CleanupStore
	Encryptor         *encryption.Encryptor
	Reconciler        Reconciler
	MultipartBackfill MultipartBackfiller
	Token             string
	LogLevel          *slog.LevelVar
}

// New creates a new admin API handler from its narrow dependency bag.
func New(d *Deps) *Handler {
	return &Handler{
		log: slog.Default().With(logfmt.Component("admin")),
		backendOps:        d.BackendOps,
		replicator:        d.Replicator,
		overRep:           d.OverRep,
		drain:             d.Drain,
		scrubber:          d.Scrubber,
		lifecycle:         d.Lifecycle,
		reconciler:        d.Reconciler,
		multipartBackfill: d.MultipartBackfill,
		dbHealthy:         d.DBHealthy,
		objects:           d.Objects,
		cleanup:           d.Cleanup,
		encAdmin:          d.Encryption,
		encryptor:         d.Encryptor,
		token:             d.Token,
		logLevel:          d.LogLevel,
	}
}

// Register mounts the admin API routes on the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/status", h.requireToken(h.handleStatus))
	mux.HandleFunc("GET /admin/api/object-locations", h.requireToken(h.handleObjectLocations))
	mux.HandleFunc("GET /admin/api/cleanup-queue", h.requireToken(h.handleCleanupQueue))
	mux.HandleFunc("POST /admin/api/usage-flush", h.requireToken(h.handleUsageFlush))
	mux.HandleFunc("POST /admin/api/replicate", h.requireToken(h.handleReplicate))
	mux.HandleFunc("GET /admin/api/log-level", h.requireToken(h.handleLogLevel))
	mux.HandleFunc("PUT /admin/api/log-level", h.requireToken(h.handleLogLevel))
	mux.HandleFunc("POST /admin/api/backends/{name}/drain", h.requireToken(h.handleStartDrain))
	mux.HandleFunc("GET /admin/api/backends/{name}/drain", h.requireToken(h.handleDrainProgress))
	mux.HandleFunc("DELETE /admin/api/backends/{name}/drain", h.requireToken(h.handleCancelDrain))
	mux.HandleFunc("DELETE /admin/api/backends/{name}", h.requireToken(h.handleRemoveBackend))
	mux.HandleFunc("GET /admin/api/over-replication", h.requireToken(h.handleOverReplicationStatus))
	mux.HandleFunc("POST /admin/api/over-replication", h.requireToken(h.handleOverReplicationClean))
	mux.HandleFunc("POST /admin/api/rotate-encryption-key", h.requireToken(h.handleRotateEncryptionKey))
	mux.HandleFunc("POST /admin/api/encrypt-existing", h.requireToken(h.handleEncryptExisting))
	mux.HandleFunc("POST /admin/api/decrypt-existing", h.requireToken(h.handleDecryptExisting))
	mux.HandleFunc("POST /admin/api/scrub", h.requireToken(h.handleScrub))
	mux.HandleFunc("POST /admin/api/backfill-checksums", h.requireToken(h.handleBackfillChecksums))
	mux.HandleFunc("POST /admin/api/reconcile", h.requireToken(h.handleReconcile))
	mux.HandleFunc("POST /admin/api/multipart-dek-backfill", h.requireToken(h.handleMultipartDEKBackfill))
}

// -------------------------------------------------------------------------
// AUTH MIDDLEWARE
// -------------------------------------------------------------------------

// requireToken wraps a handler and enforces X-Admin-Token authentication.
func (h *Handler) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) != 1 {
			h.log.WarnContext(r.Context(), "unauthorized request", "path", r.URL.Path, "client_addr", r.RemoteAddr)
			errorJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// -------------------------------------------------------------------------
// HANDLERS
// -------------------------------------------------------------------------

// handleStatus returns backend health and circuit breaker state.
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	data, err := h.backendOps.GetDashboardData(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch status", err)
		return
	}

	type backendStatus struct {
		Name         string `json:"name"`
		BytesUsed    int64  `json:"bytes_used"`
		BytesLimit   int64  `json:"bytes_limit"`
		ObjectCount  int64  `json:"object_count"`
		APIRequests  int64  `json:"api_requests"`
		EgressBytes  int64  `json:"egress_bytes"`
		IngressBytes int64  `json:"ingress_bytes"`
	}

	backends := make([]backendStatus, 0, len(data.BackendOrder))
	for _, name := range data.BackendOrder {
		bs := backendStatus{Name: name}
		if qs, ok := data.QuotaStats[name]; ok {
			bs.BytesUsed = qs.BytesUsed
			bs.BytesLimit = qs.BytesLimit
		}
		if oc, ok := data.ObjectCounts[name]; ok {
			bs.ObjectCount = oc
		}
		if us, ok := data.UsageStats[name]; ok {
			bs.APIRequests = us.APIRequests
			bs.EgressBytes = us.EgressBytes
			bs.IngressBytes = us.IngressBytes
		}
		backends = append(backends, bs)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"db_healthy":   h.dbHealthy(),
		"backends":     backends,
		"usage_period": data.UsagePeriod,
	})
}

// handleObjectLocations returns all copies of an object across backends.
func (h *Handler) handleObjectLocations(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		errorJSON(w, http.StatusBadRequest, "key parameter is required")
		return
	}

	locations, err := h.objects.GetAllObjectLocations(r.Context(), key)
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch locations", err, slog.String("key", key))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"key": key, "locations": locations})
}

// handleCleanupQueue returns cleanup queue depth and pending items.
func (h *Handler) handleCleanupQueue(w http.ResponseWriter, r *http.Request) {
	depth, err := h.cleanup.CleanupQueueDepth(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch cleanup queue", err)
		return
	}

	items, err := h.cleanup.GetPendingCleanups(r.Context(), 50)
	if err != nil {
		h.internalError(r.Context(), w, "failed to fetch cleanup queue", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"depth": depth, "items": items})
}

// handleUsageFlush forces a flush of usage counters to the database.
func (h *Handler) handleUsageFlush(w http.ResponseWriter, r *http.Request) {
	if err := h.backendOps.FlushUsage(r.Context()); err != nil {
		h.internalError(r.Context(), w, "flush failed", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "flushed"})
}

// ReplicateResult is the outcome of a one-shot replication cycle.
type ReplicateResult struct {
	Status        string // "ok" or "skipped"
	Reason        string // populated when Status is "skipped"
	CopiesCreated int
}

// Replicate runs one replication cycle synchronously and returns the
// resulting counts. Skips when replication is unconfigured or factor <= 1.
// Refreshes quota metrics on success. Exposed for callers (UI, tests) that
// need the counts back as Go values rather than JSON.
func (h *Handler) Replicate(ctx context.Context) (ReplicateResult, error) {
	rcfg := h.replicator.Config()
	if rcfg == nil || rcfg.Factor <= 1 {
		return ReplicateResult{Status: "skipped", Reason: "replication not configured or factor <= 1"}, nil
	}

	created, err := h.replicator.Replicate(ctx, *rcfg)
	if err != nil {
		return ReplicateResult{}, err
	}

	if mErr := h.backendOps.UpdateQuotaMetrics(ctx); mErr != nil {
		h.log.WarnContext(ctx, "failed to update quota metrics after replicate", "error", mErr)
	}

	return ReplicateResult{Status: "ok", CopiesCreated: created}, nil
}

// handleReplicate triggers one replication cycle.
func (h *Handler) handleReplicate(w http.ResponseWriter, r *http.Request) {
	res, err := h.Replicate(r.Context())
	if err != nil {
		h.internalError(r.Context(), w, "replication failed", err)
		return
	}

	if res.Status == "skipped" {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "skipped",
			"copies_created": 0,
			"reason":         res.Reason,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "copies_created": res.CopiesCreated})
}

// handleOverReplicationStatus returns the count of over-replicated objects.
func (h *Handler) handleOverReplicationStatus(w http.ResponseWriter, r *http.Request) {
	rcfg := h.overRep.Config()
	if rcfg == nil || rcfg.Factor <= 1 {
		writeJSON(w, http.StatusOK, map[string]any{
			"factor":  0,
			"pending": 0,
			"status":  "replication not configured",
		})
		return
	}

	count, err := h.overRep.CountPending(r.Context(), rcfg.Factor)
	if err != nil {
		h.internalError(r.Context(), w, "failed to count over-replicated objects", err)
		return
	}

	telemetry.OverReplicationPending.Set(float64(count))
	writeJSON(w, http.StatusOK, map[string]any{
		"factor":  rcfg.Factor,
		"pending": count,
	})
}

// handleOverReplicationClean triggers an immediate over-replication cleanup pass.
func (h *Handler) handleOverReplicationClean(w http.ResponseWriter, r *http.Request) {
	rcfg := h.overRep.Config()
	if rcfg == nil || rcfg.Factor <= 1 {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "skipped",
			"copies_removed": 0,
			"reason":         "replication not configured or factor <= 1",
		})
		return
	}

	// Allow callers to override batch size via query parameter.
	cfg := *rcfg
	if bs := r.URL.Query().Get("batch_size"); bs != "" {
		if n, err := strconv.Atoi(bs); err == nil && n > 0 {
			if n > 10000 {
				n = 10000
			}
			cfg.BatchSize = n
		}
	}

	removed, err := h.overRep.Clean(r.Context(), cfg)
	if err != nil {
		h.internalError(r.Context(), w, "over-replication cleanup failed", err)
		return
	}

	if err := h.backendOps.UpdateQuotaMetrics(r.Context()); err != nil {
		h.log.WarnContext(r.Context(), "failed to update quota metrics after admin over-replication cleanup", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "copies_removed": removed})
}

// handleLogLevel gets or sets the runtime log level.
func (h *Handler) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{"level": strings.ToLower(h.logLevel.Level().String())})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorJSON(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	parsed := config.ParseLogLevel(req.Level)
	h.logLevel.Set(parsed)
	h.log.InfoContext(r.Context(), "log level changed via admin API", "level", req.Level)
	writeJSON(w, http.StatusOK, map[string]string{"level": strings.ToLower(parsed.String())})
}

// -------------------------------------------------------------------------
// BACKEND LIFECYCLE HANDLERS
// -------------------------------------------------------------------------

// handleStartDrain begins draining a backend.
func (h *Handler) handleStartDrain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.drain.StartDrain(r.Context(), name); err != nil {
		h.log.ErrorContext(r.Context(), "drain start failed", slog.String("backend", name), "error", err)
		errorJSON(w, http.StatusBadRequest, errDrainOperationFailed)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "drain started", "backend": name})
}

// handleDrainProgress returns the current state of a drain operation.
func (h *Handler) handleDrainProgress(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	progress, err := h.drain.GetDrainProgress(r.Context(), name)
	if err != nil {
		h.log.ErrorContext(r.Context(), "drain progress failed", slog.String("backend", name), "error", err)
		errorJSON(w, http.StatusInternalServerError, errDrainOperationFailed)
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

// handleCancelDrain cancels an active drain operation.
func (h *Handler) handleCancelDrain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.drain.CancelDrain(name); err != nil {
		h.log.ErrorContext(r.Context(), "drain cancel failed", slog.String("backend", name), "error", err)
		errorJSON(w, http.StatusBadRequest, errDrainOperationFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "drain cancelled", "backend": name})
}

// removeConfirmTTL is how long a purge confirmation token is valid.
const (
	removeConfirmTTL        = 60 * time.Second
	errEncryptionNotEnabled = "encryption not enabled"
	errDrainOperationFailed = "drain operation failed"
)

// handleRemoveBackend deletes all DB records for a backend. When purge=true,
// requires two-phase confirmation: first call returns a preview with a signed
// token, second call with confirm=<token> executes the purge.
// Without purge, executes immediately (DB records only, S3 objects preserved).
func (h *Handler) handleRemoveBackend(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	purge := r.URL.Query().Get("purge") == "true"
	confirmToken := r.URL.Query().Get("confirm")

	// Non-purge removal: drop DB records immediately (reversible via sync)
	if !purge {
		if err := h.drain.RemoveBackend(r.Context(), name, false); err != nil {
			h.log.ErrorContext(r.Context(), "remove backend failed", slog.String("backend", name), "error", err)
			errorJSON(w, http.StatusBadRequest, "remove failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "backend removed", "backend": name})
		return
	}

	// Purge phase 2: validate token and execute
	if confirmToken != "" {
		if !h.validRemoveToken(confirmToken, name) {
			errorJSON(w, http.StatusForbidden, "invalid or expired confirmation token")
			return
		}
		if err := h.drain.RemoveBackend(r.Context(), name, true); err != nil {
			h.log.ErrorContext(r.Context(), "purge backend failed", slog.String("backend", name), "error", err)
			errorJSON(w, http.StatusBadRequest, "purge failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "backend purged", "backend": name})
		return
	}

	// Purge phase 1: preview what will be destroyed, return confirmation token
	objectCount, totalBytes, err := h.lifecycle.BackendObjectStats(r.Context(), name)
	if err != nil {
		errorJSON(w, http.StatusBadRequest, "backend not found or stats unavailable")
		return
	}

	token := h.generateRemoveToken(name)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "confirmation required",
		"backend":       name,
		"object_count":  objectCount,
		"total_bytes":   totalBytes,
		"confirm_token": token,
		"expires_in":    int(removeConfirmTTL.Seconds()),
	})
}

// generateRemoveToken creates an HMAC-signed token encoding the backend name
// and expiry. Uses the admin token as the HMAC key.
func (h *Handler) generateRemoveToken(name string) string {
	expiry := time.Now().Add(removeConfirmTTL).Unix()
	payload := fmt.Sprintf("purge|%s|%d", name, expiry)
	mac := hmac.New(sha256.New, []byte(h.token))
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
}

// validRemoveToken verifies a purge confirmation token.
func (h *Handler) validRemoveToken(token, expectedName string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(h.token))
	mac.Write(payloadBytes)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return false
	}

	fields := strings.SplitN(string(payloadBytes), "|", 3)
	if len(fields) != 3 || fields[0] != "purge" || fields[1] != expectedName {
		return false
	}
	expiry, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < expiry
}

// -------------------------------------------------------------------------
// ENCRYPTION KEY ROTATION
// -------------------------------------------------------------------------

// handleRotateEncryptionKey re-wraps all encrypted objects' DEKs with
// the current primary key. Objects are processed in batches to avoid
// holding long transactions. The old key must remain in previous_keys
// for unwrapping.
func (h *Handler) handleRotateEncryptionKey(w http.ResponseWriter, r *http.Request) {
	if h.encryptor == nil || h.encAdmin == nil {
		errorJSON(w, http.StatusBadRequest, errEncryptionNotEnabled)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		OldKeyID string `json:"old_key_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OldKeyID == "" {
		errorJSON(w, http.StatusBadRequest, "old_key_id is required")
		return
	}

	ctx := r.Context()
	const batchSize = 500
	var rotated, failed, total int

	for offset := 0; ; offset += batchSize {
		locs, err := h.encAdmin.ListEncryptedLocations(ctx, req.OldKeyID, batchSize, offset)
		if err != nil {
			h.log.ErrorContext(ctx, "key rotation list failed", "error", err)
			errorJSON(w, http.StatusInternalServerError, "failed to list encrypted objects")
			return
		}
		if len(locs) == 0 {
			break
		}
		batchRotated, batchFailed := h.rotateBatch(ctx, locs)
		rotated += batchRotated
		failed += batchFailed
		total += len(locs)
		if len(locs) < batchSize {
			break
		}
	}

	h.log.InfoContext(ctx, "key rotation complete", "rotated", rotated, "failed", failed, "total", total)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "complete",
		"rotated": rotated,
		"failed":  failed,
		"total":   total,
	})
}

// rotateBatch re-wraps the DEKs for one paginated batch of encrypted
// locations, returning the per-batch success and failure counts.
func (h *Handler) rotateBatch(ctx context.Context, locs []core.EncryptedLocation) (rotated, failed int) {
	for _, loc := range locs {
		if h.rotateOneLocation(ctx, loc) {
			rotated++
		} else {
			failed++
		}
	}
	return rotated, failed
}

// rotateOneLocation re-wraps the DEK for a single encrypted location.
// Returns true on success; logs and increments error telemetry on
// failure (every failure mode is non-fatal so the batch can continue).
func (h *Handler) rotateOneLocation(ctx context.Context, loc core.EncryptedLocation) bool {
	baseNonce, wrappedDEK, unpackErr := encryption.UnpackKeyData(loc.EncryptionKey)
	if unpackErr != nil {
		h.log.WarnContext(ctx, "unpack failed", slog.String("key", loc.ObjectKey), "error", unpackErr)
		telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
		return false
	}

	dek, unwrapErr := h.encryptor.Provider().UnwrapDEK(ctx, wrappedDEK, loc.KeyID)
	if unwrapErr != nil {
		h.log.WarnContext(ctx, "unwrap failed", "key", loc.ObjectKey, "error", unwrapErr)
		telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
		return false
	}

	newWrapped, newKeyID, wrapErr := h.encryptor.Provider().WrapDEK(ctx, dek)
	if wrapErr != nil {
		h.log.WarnContext(ctx, "wrap failed", "key", loc.ObjectKey, "error", wrapErr)
		telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
		return false
	}

	newKeyData := encryption.PackKeyData(baseNonce, newWrapped)
	if err := h.encAdmin.UpdateEncryptionKey(ctx, loc.ObjectKey, loc.BackendName, newKeyData, newKeyID); err != nil {
		h.log.WarnContext(ctx, "update failed", "key", loc.ObjectKey, "error", err)
		telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
		return false
	}

	telemetry.KeyRotationObjectsTotal.WithLabelValues("success").Inc()
	return true
}

// -------------------------------------------------------------------------
// ENCRYPT / DECRYPT EXISTING OBJECTS
// -------------------------------------------------------------------------

// bulkRewriteRow is the minimum surface every row passed to the bulk-rewrite
// driver must expose. Both store.UnencryptedLocation and
// store.DecryptableLocation satisfy it.
type bulkRewriteRow interface {
	rewriteKey() string
	rewriteBackend() string
	rewriteSize() int64
}

// adapter wrappers (unexported) keep the handlers free of accessor noise
// while letting bulkRewriteRow stay independent of the store package types.
// Pointer receivers avoid copying the embedded store row (DecryptableLocation
// carries a []byte and trips gocritic's hugeParam).
type encryptRow struct{ core.UnencryptedLocation }

// rewriteKey returns the object key the bulk-rewrite loop should
// re-process. Implements the rewriteRow interface for encryptRow.
func (r *encryptRow) rewriteKey() string { return r.ObjectKey }

// rewriteBackend returns the source backend the row currently lives on.
// Implements the rewriteRow interface for encryptRow.
func (r *encryptRow) rewriteBackend() string { return r.BackendName }

// rewriteSize returns the row's stored size, used for quota accounting
// and progress reporting. Implements rewriteRow for encryptRow.
func (r *encryptRow) rewriteSize() int64 { return r.SizeBytes }

// decryptRow wraps core.DecryptableLocation so the same bulkRewriteRow
// machinery that handles encrypt-existing can run decrypt-existing
// without duplicating the pagination + download + upload + DB-update
// scaffolding.
type decryptRow struct{ core.DecryptableLocation }

// rewriteKey returns the object key to re-process. Implements
// rewriteRow for decryptRow.
func (r *decryptRow) rewriteKey() string { return r.ObjectKey }

// rewriteBackend returns the source backend the row currently lives on.
// Implements rewriteRow for decryptRow.
func (r *decryptRow) rewriteBackend() string { return r.BackendName }

// rewriteSize returns the row's stored size, used for quota accounting
// and progress reporting. Implements rewriteRow for decryptRow.
func (r *decryptRow) rewriteSize() int64 { return r.SizeBytes }

// bulkRewriteOp parameterises the encrypt/decrypt-existing handlers, which
// share their pagination + download + upload + DB-update scaffolding and
// differ only in the listing query, the transform step, and the result
// label.
type bulkRewriteOp[L bulkRewriteRow] struct {
	opName      string // "encrypt-existing" / "decrypt-existing"
	logTag      string // "Encrypt-existing" / "Decrypt-existing"
	listErrMsg  string
	resultLabel string // "encrypted" / "decrypted"
	counter     *prometheus.CounterVec
	listFn      func(ctx context.Context, batchSize, offset int) ([]L, error)
	// rewrite consumes a downloaded object and returns the bytes to re-upload,
	// the size to record as PUT egress, and a closure that performs the DB
	// metadata update on success. Implementations are responsible for closing
	// the source body if they fail before returning the new reader.
	rewrite func(ctx context.Context, src *backend.GetObjectResult, loc L) (io.Reader, int64, func() error, error)
}

// BulkRewriteResult is the outcome of a bulk encrypt/decrypt-existing pass.
// Status is "complete" when the run finished, "skipped" when the operation
// is unavailable (encryption not enabled).
type BulkRewriteResult struct {
	Status  string
	Reason  string
	Success int
	Failed  int
	Total   int
}

// EncryptExisting downloads every unencrypted object, encrypts it,
// re-uploads the ciphertext, and updates the DB record. Returns counts.
// Skips when encryption is not configured.
func (h *Handler) EncryptExisting(ctx context.Context) BulkRewriteResult {
	return runBulkRewriteCounts(h, ctx, bulkRewriteOp[*encryptRow]{
		opName:      "encrypt-existing",
		logTag:      "Encrypt-existing",
		listErrMsg:  "failed to list unencrypted objects",
		resultLabel: "encrypted",
		counter:     telemetry.EncryptExistingObjectsTotal,
		listFn: func(ctx context.Context, batchSize, offset int) ([]*encryptRow, error) {
			rows, err := h.encAdmin.ListUnencryptedLocations(ctx, batchSize, offset)
			if err != nil {
				return nil, err
			}
			out := make([]*encryptRow, len(rows))
			for i := range rows {
				out[i] = &encryptRow{rows[i]}
			}
			return out, nil
		},
		rewrite: func(ctx context.Context, src *backend.GetObjectResult, loc *encryptRow) (io.Reader, int64, func() error, error) {
			encResult, err := h.encryptor.Encrypt(ctx, src.Body, loc.SizeBytes)
			if err != nil {
				return nil, 0, nil, err
			}
			dbUpdate := func() error {
				keyData := encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK)
				return h.encAdmin.MarkObjectEncrypted(ctx, loc.ObjectKey, loc.BackendName, keyData, encResult.KeyID, loc.SizeBytes, encResult.CiphertextSize)
			}
			return encResult.Body, encResult.CiphertextSize, dbUpdate, nil
		},
	})
}

// handleEncryptExisting wraps EncryptExisting in JSON envelope semantics.
func (h *Handler) handleEncryptExisting(w http.ResponseWriter, r *http.Request) {
	res := h.EncryptExisting(r.Context())
	if res.Status == "skipped" {
		errorJSON(w, http.StatusBadRequest, res.Reason)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "complete",
		"encrypted": res.Success,
		"failed":    res.Failed,
		"total":     res.Total,
	})
}

// handleDecryptExisting downloads each encrypted object from its backend,
// decrypts it, re-uploads the plaintext, and updates the DB record. Objects
// are processed in batches to avoid holding long transactions. Encryption
// must still be configured (the key provider is needed to unwrap DEKs).
func (h *Handler) handleDecryptExisting(w http.ResponseWriter, r *http.Request) {
	res := runBulkRewriteCounts(h, r.Context(), bulkRewriteOp[*decryptRow]{
		opName:      "decrypt-existing",
		logTag:      "Decrypt-existing",
		listErrMsg:  "failed to list encrypted objects",
		resultLabel: "decrypted",
		counter:     telemetry.DecryptExistingObjectsTotal,
		listFn: func(ctx context.Context, batchSize, offset int) ([]*decryptRow, error) {
			rows, err := h.encAdmin.ListAllEncryptedLocations(ctx, batchSize, offset)
			if err != nil {
				return nil, err
			}
			out := make([]*decryptRow, len(rows))
			for i := range rows {
				out[i] = &decryptRow{rows[i]}
			}
			return out, nil
		},
		rewrite: func(ctx context.Context, src *backend.GetObjectResult, loc *decryptRow) (io.Reader, int64, func() error, error) {
			_, wrappedDEK, unpackErr := encryption.UnpackKeyData(loc.EncryptionKey)
			if unpackErr != nil {
				return nil, 0, nil, fmt.Errorf("unpack key data: %w", unpackErr)
			}
			plainReader, err := h.encryptor.Decrypt(ctx, src.Body, wrappedDEK, loc.KeyID)
			if err != nil {
				return nil, 0, nil, err
			}
			dbUpdate := func() error {
				return h.encAdmin.MarkObjectDecrypted(ctx, loc.ObjectKey, loc.BackendName, loc.PlaintextSize)
			}
			return plainReader, loc.PlaintextSize, dbUpdate, nil
		},
	})
	if res.Status == "skipped" {
		errorJSON(w, http.StatusBadRequest, res.Reason)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "complete",
		"decrypted": res.Success,
		"failed":    res.Failed,
		"total":     res.Total,
	})
}

// runBulkRewriteCounts is the shared driver for the encrypt/decrypt-existing
// operations. Returns counts so callers (HTTP handlers, UI handlers, tests)
// can decide how to surface them. A list-listFn error short-circuits the
// run with the partial counts gathered so far.
func runBulkRewriteCounts[L bulkRewriteRow](h *Handler, ctx context.Context, op bulkRewriteOp[L]) BulkRewriteResult {
	if h.encryptor == nil || h.encAdmin == nil {
		return BulkRewriteResult{Status: "skipped", Reason: errEncryptionNotEnabled}
	}

	const batchSize = 100
	var success, failed, total int

	for offset := 0; ; offset += batchSize {
		rows, err := op.listFn(ctx, batchSize, offset)
		if err != nil {
			h.log.ErrorContext(ctx, "Admin: "+op.opName+" list failed", "error", err)
			return BulkRewriteResult{Status: "skipped", Reason: op.listErrMsg, Success: success, Failed: failed, Total: total}
		}
		if len(rows) == 0 {
			break
		}

		for _, row := range rows {
			total++
			if processBulkLocation(h, ctx, op, row) {
				success++
			} else {
				failed++
			}
		}

		if len(rows) < batchSize {
			break
		}
	}

	h.log.InfoContext(ctx, "Admin: "+op.opName+" complete", op.resultLabel, success, "failed", failed, "total", total)
	return BulkRewriteResult{Status: "complete", Success: success, Failed: failed, Total: total}
}

// processBulkLocation runs one rewrite step for a single object location:
// download from backend, transform via op.rewrite, upload, record usage,
// and run the DB update. Returns true on success, false on any failure
// (which is already logged and metric-recorded).
func processBulkLocation[L bulkRewriteRow](h *Handler, ctx context.Context, op bulkRewriteOp[L], loc L) bool {
	key, backendName, sizeBytes := loc.rewriteKey(), loc.rewriteBackend(), loc.rewriteSize()

	be, err := h.backendOps.GetBackend(backendName)
	if err != nil {
		h.log.WarnContext(ctx, op.logTag+": backend not found", "key", key, "backend", backendName, "error", err)
		op.counter.WithLabelValues("error").Inc()
		return false
	}

	src, err := be.GetObject(ctx, key, "")
	if err != nil {
		h.backendOps.RecordUsage(backendName, 1, 0, 0)
		h.log.WarnContext(ctx, op.logTag+": download failed", "key", key, "backend", backendName, "error", err)
		op.counter.WithLabelValues("error").Inc()
		return false
	}
	h.backendOps.RecordUsage(backendName, 1, sizeBytes, 0)

	body, putSize, dbUpdate, err := op.rewrite(ctx, src, loc)
	if err != nil {
		src.Body.Close()
		h.log.WarnContext(ctx, op.logTag+": transform failed", "key", key, "error", err)
		op.counter.WithLabelValues("error").Inc()
		return false
	}

	_, err = be.PutObject(ctx, key, body, putSize, src.ContentType, src.Metadata)
	src.Body.Close()
	if err != nil {
		h.backendOps.RecordUsage(backendName, 1, 0, 0)
		h.log.WarnContext(ctx, op.logTag+": re-upload failed", "key", key, "backend", backendName, "error", err)
		op.counter.WithLabelValues("error").Inc()
		return false
	}
	h.backendOps.RecordUsage(backendName, 1, 0, putSize)

	if err := dbUpdate(); err != nil {
		h.log.WarnContext(ctx, op.logTag+": DB update failed", "key", key, "error", err)
		op.counter.WithLabelValues("error").Inc()
		return false
	}

	op.counter.WithLabelValues("success").Inc()
	return true
}

// -------------------------------------------------------------------------
// INTEGRITY
// -------------------------------------------------------------------------

// ScrubResult is the outcome of one on-demand scrub cycle.
type ScrubResult struct {
	Status  string // "ok" or "skipped"
	Reason  string // populated when Status is "skipped"
	Checked int
	Failed  int
}

// Scrub runs one integrity-verification scrub pass synchronously and
// returns the per-pass counts. batchSize <= 0 means use the configured
// ScrubberBatchSize. Skips when integrity verification is not enabled.
func (h *Handler) Scrub(ctx context.Context, batchSize int) ScrubResult {
	icfg := h.backendOps.IntegrityConfig()
	if icfg == nil || !icfg.Enabled {
		return ScrubResult{Status: "skipped", Reason: "integrity verification is not enabled"}
	}
	if batchSize <= 0 {
		batchSize = icfg.ScrubberBatchSize
	}
	checked, failed := h.scrubber.Scrub(ctx, batchSize)
	return ScrubResult{Status: "ok", Checked: checked, Failed: failed}
}

// handleScrub triggers an on-demand scrub cycle. Accepts an optional
// batch_size query parameter (defaults to the configured scrubber batch size).
func (h *Handler) handleScrub(w http.ResponseWriter, r *http.Request) {
	batchSize := 0
	if bs := r.URL.Query().Get("batch_size"); bs != "" {
		if v, err := strconv.ParseInt(bs, 10, 32); err == nil && v > 0 {
			batchSize = int(v)
		}
	}

	res := h.Scrub(r.Context(), batchSize)
	if res.Status == "skipped" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "skipped", "reason": res.Reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"checked": res.Checked,
		"failed":  res.Failed,
	})
}

// BackfillChecksumsResult is the outcome of a checksum backfill pass.
type BackfillChecksumsResult struct {
	Status    string // "ok" or "skipped"
	Reason    string // populated when Status is "skipped"
	Processed int
}

// BackfillChecksums computes and stores content hashes for objects that
// don't have one, paginating internally until all objects are processed
// or the context is cancelled. batchSize <= 0 means use 100. Skips when
// integrity verification is not enabled.
func (h *Handler) BackfillChecksums(ctx context.Context, batchSize int) BackfillChecksumsResult {
	icfg := h.backendOps.IntegrityConfig()
	if icfg == nil || !icfg.Enabled {
		return BackfillChecksumsResult{Status: "skipped", Reason: "integrity verification is not enabled"}
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	h.log.InfoContext(ctx, "Backfill-checksums started", "batch_size", batchSize)
	var total int
	for offset := 0; ; {
		processed, nextOffset := h.scrubber.Backfill(ctx, batchSize, offset)
		total += processed
		if nextOffset == 0 {
			break
		}
		offset = nextOffset
	}
	return BackfillChecksumsResult{Status: "ok", Processed: total}
}

// handleBackfillChecksums triggers a checksum backfill pass.
func (h *Handler) handleBackfillChecksums(w http.ResponseWriter, r *http.Request) {
	batchSize := 0
	if bs := r.URL.Query().Get("batch_size"); bs != "" {
		if v, err := strconv.ParseInt(bs, 10, 32); err == nil && v > 0 {
			batchSize = int(v)
		}
	}

	res := h.BackfillChecksums(r.Context(), batchSize)
	if res.Status == "skipped" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "skipped", "reason": res.Reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "processed": res.Processed})
}

// handleMultipartDEKBackfill triggers an on-demand pass of the legacy
// multipart-upload DEK backfill worker. The startup hook in cli/serve
// already runs one pass before the listener accepts traffic; this
// endpoint exists for operators who need to rerun after fixing a
// transient failure (backend outage, exhausted page budget, etc.).
// Response: {"status":"ok","migrated":N} on success, 503 when the
// worker is not configured (encryption disabled).
func (h *Handler) handleMultipartDEKBackfill(w http.ResponseWriter, r *http.Request) {
	if h.multipartBackfill == nil {
		errorJSON(w, http.StatusServiceUnavailable, "encryption is not enabled")
		return
	}
	h.log.InfoContext(r.Context(), "multipart DEK backfill triggered")
	migrated, err := h.multipartBackfill.RunOnce(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "multipart DEK backfill failed",
			"migrated", migrated, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":    err.Error(),
			"migrated": migrated,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"migrated": migrated,
	})
}

// handleReconcile triggers an on-demand reconciliation. Lists objects on
// each backend, diffs against DB entries, imports untracked objects, and
// removes stale entries. Use ?backend=name to scope to a single backend.
func (h *Handler) handleReconcile(w http.ResponseWriter, r *http.Request) {
	backendName := r.URL.Query().Get("backend")

	if h.reconciler == nil {
		errorJSON(w, http.StatusServiceUnavailable, "reconciler not configured")
		return
	}

	h.log.InfoContext(r.Context(), "reconcile triggered", "backend", backendName)

	result, err := h.reconciler.Reconcile(r.Context(), backendName)
	if err != nil {
		h.log.ErrorContext(r.Context(), "reconcile failed", "error", err)
		errorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.log.InfoContext(r.Context(), "reconcile complete",
		"imported", result.Imported, "removed", result.Removed,
		"backends_scanned", result.BackendsScanned)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"imported":         result.Imported,
		"removed":          result.Removed,
		"backends_scanned": result.BackendsScanned,
	})
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode JSON response", "error", err) //nolint:sloglint,gosec // standalone helper, no request context; err is internal encoder failure, not user-controlled
	}
}

// errorJSON writes a JSON error envelope ({"error": msg}) with the given
// status code. Use directly for client-side errors (4xx, 503) where the
// underlying cause is not interesting to log.
func errorJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// internalError logs the underlying error against the operator-facing
// message and writes a 500 JSON response. Use for unexpected server-side
// failures where the original err is recorded but never returned to the
// caller. Extra attrs are appended to the log call so the caller can
// attach correlating fields (object key, backend name, etc.).
func (h *Handler) internalError(ctx context.Context, w http.ResponseWriter, msg string, err error, attrs ...any) {
	args := append([]any{"error", err}, attrs...)
	h.log.ErrorContext(ctx, msg, args...)
	errorJSON(w, http.StatusInternalServerError, msg)
}
