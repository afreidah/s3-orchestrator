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
	"crypto/subtle"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"

	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"

	// Handler serves the admin API endpoints.
	"github.com/afreidah/s3-orchestrator/internal/store"
)

type Handler struct {
	manager   *proxy.BackendManager
	store     *store.CircuitBreakerStore
	rawStore  *store.Store
	encryptor *encryption.Encryptor
	token     string
	logLevel  *slog.LevelVar
}

// New creates a new admin API handler.
func New(manager *proxy.BackendManager, store *store.CircuitBreakerStore, rawStore *store.Store, encryptor *encryption.Encryptor, token string, logLevel *slog.LevelVar) *Handler {
	return &Handler{
		manager:   manager,
		store:     store,
		rawStore:  rawStore,
		encryptor: encryptor,
		token:     token,
		logLevel:  logLevel,
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
	mux.HandleFunc("POST /admin/api/scrub", h.requireToken(h.handleScrub))
	mux.HandleFunc("POST /admin/api/backfill-checksums", h.requireToken(h.handleBackfillChecksums))
}

// -------------------------------------------------------------------------
// AUTH MIDDLEWARE
// -------------------------------------------------------------------------

// requireToken wraps a handler and enforces X-Admin-Token authentication.
func (h *Handler) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) != 1 {
			slog.WarnContext(r.Context(), "Admin: unauthorized request", "path", r.URL.Path, "remote", r.RemoteAddr)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
	data, err := h.manager.GetDashboardData(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "Admin: failed to fetch status", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch status"})
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
		"db_healthy":   h.store.IsHealthy(),
		"backends":     backends,
		"usage_period": data.UsagePeriod,
	})
}

// handleObjectLocations returns all copies of an object across backends.
func (h *Handler) handleObjectLocations(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "key parameter is required"})
		return
	}

	locations, err := h.store.GetAllObjectLocations(r.Context(), key)
	if err != nil {
		slog.ErrorContext(r.Context(), "Admin: failed to fetch object locations", "key", key, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch locations"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"key": key, "locations": locations})
}

// handleCleanupQueue returns cleanup queue depth and pending items.
func (h *Handler) handleCleanupQueue(w http.ResponseWriter, r *http.Request) {
	depth, err := h.store.CleanupQueueDepth(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "Admin: failed to fetch cleanup queue depth", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch cleanup queue"})
		return
	}

	items, err := h.store.GetPendingCleanups(r.Context(), 50)
	if err != nil {
		slog.ErrorContext(r.Context(), "Admin: failed to fetch pending cleanups", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch cleanup queue"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"depth": depth, "items": items})
}

// handleUsageFlush forces a flush of usage counters to the database.
func (h *Handler) handleUsageFlush(w http.ResponseWriter, r *http.Request) {
	if err := h.manager.FlushUsage(r.Context()); err != nil {
		slog.ErrorContext(r.Context(), "Admin: usage flush failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "flush failed"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "flushed"})
}

// handleReplicate triggers one replication cycle.
func (h *Handler) handleReplicate(w http.ResponseWriter, r *http.Request) {
	rcfg := h.manager.Replicator.Config()
	if rcfg == nil || rcfg.Factor <= 1 {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "skipped",
			"copies_created": 0,
			"reason":         "replication not configured or factor <= 1",
		})
		return
	}

	created, err := h.manager.Replicator.Replicate(r.Context(), *rcfg)
	if err != nil {
		slog.ErrorContext(r.Context(), "Admin: replication failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "replication failed"})
		return
	}

	if err := h.manager.UpdateQuotaMetrics(r.Context()); err != nil {
		slog.WarnContext(r.Context(), "Failed to update quota metrics after admin replicate", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "copies_created": created})
}

// handleOverReplicationStatus returns the count of over-replicated objects.
func (h *Handler) handleOverReplicationStatus(w http.ResponseWriter, r *http.Request) {
	rcfg := h.manager.OverReplicationCleaner.Config()
	if rcfg == nil || rcfg.Factor <= 1 {
		writeJSON(w, http.StatusOK, map[string]any{
			"factor":  0,
			"pending": 0,
			"status":  "replication not configured",
		})
		return
	}

	count, err := h.manager.OverReplicationCleaner.CountPending(r.Context(), rcfg.Factor)
	if err != nil {
		slog.ErrorContext(r.Context(), "Admin: failed to count over-replicated objects", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to count over-replicated objects"})
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
	rcfg := h.manager.OverReplicationCleaner.Config()
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
			if n > math.MaxInt32 {
				n = math.MaxInt32
			}
			cfg.BatchSize = n
		}
	}

	removed, err := h.manager.OverReplicationCleaner.Clean(r.Context(), cfg)
	if err != nil {
		slog.ErrorContext(r.Context(), "Admin: over-replication cleanup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "over-replication cleanup failed"})
		return
	}

	if err := h.manager.UpdateQuotaMetrics(r.Context()); err != nil {
		slog.WarnContext(r.Context(), "Failed to update quota metrics after admin over-replication cleanup", "error", err)
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	parsed := config.ParseLogLevel(req.Level)
	h.logLevel.Set(parsed)
	slog.InfoContext(r.Context(), "Log level changed via admin API", "level", req.Level)
	writeJSON(w, http.StatusOK, map[string]string{"level": strings.ToLower(parsed.String())})
}

// -------------------------------------------------------------------------
// BACKEND LIFECYCLE HANDLERS
// -------------------------------------------------------------------------

// handleStartDrain begins draining a backend.
func (h *Handler) handleStartDrain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.manager.DrainManager.StartDrain(r.Context(), name); err != nil {
		slog.ErrorContext(r.Context(), "Admin: drain start failed", "backend", name, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "drain operation failed"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "drain started", "backend": name})
}

// handleDrainProgress returns the current state of a drain operation.
func (h *Handler) handleDrainProgress(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	progress, err := h.manager.DrainManager.GetDrainProgress(r.Context(), name)
	if err != nil {
		slog.ErrorContext(r.Context(), "Admin: drain progress failed", "backend", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "drain operation failed"})
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

// handleCancelDrain cancels an active drain operation.
func (h *Handler) handleCancelDrain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.manager.DrainManager.CancelDrain(name); err != nil {
		slog.ErrorContext(r.Context(), "Admin: drain cancel failed", "backend", name, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "drain operation failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "drain cancelled", "backend": name})
}

// removeConfirmTTL is how long a purge confirmation token is valid.
const removeConfirmTTL = 60 * time.Second

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
		if err := h.manager.DrainManager.RemoveBackend(r.Context(), name, false); err != nil {
			slog.ErrorContext(r.Context(), "Admin: remove backend failed", "backend", name, "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "remove failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "backend removed", "backend": name})
		return
	}

	// Purge phase 2: validate token and execute
	if confirmToken != "" {
		if !h.validRemoveToken(confirmToken, name) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid or expired confirmation token"})
			return
		}
		if err := h.manager.DrainManager.RemoveBackend(r.Context(), name, true); err != nil {
			slog.ErrorContext(r.Context(), "Admin: purge backend failed", "backend", name, "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "purge failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "backend purged", "backend": name})
		return
	}

	// Purge phase 1: preview what will be destroyed, return confirmation token
	objectCount, totalBytes, err := h.manager.Store().BackendObjectStats(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "backend not found or stats unavailable"})
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

// handleRotateEncryptionKey re-wraps all encrypted objects' DEKs with the
// current primary key. Objects are processed in batches to avoid holding long
// transactions. The old key must remain in previous_keys for unwrapping.
func (h *Handler) handleRotateEncryptionKey(w http.ResponseWriter, r *http.Request) {
	if h.encryptor == nil || h.rawStore == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "encryption not enabled"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		OldKeyID string `json:"old_key_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OldKeyID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "old_key_id is required"})
		return
	}

	ctx := r.Context()
	const batchSize = 500
	var rotated, failed, total int

	for offset := 0; ; offset += batchSize {
		locs, err := h.rawStore.ListEncryptedLocations(ctx, req.OldKeyID, batchSize, offset)
		if err != nil {
			slog.ErrorContext(ctx, "Admin: key rotation list failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list encrypted objects"})
			return
		}
		if len(locs) == 0 {
			break
		}

		for _, loc := range locs {
			total++
			// Unpack old nonce + wrapped DEK
			baseNonce, wrappedDEK, unpackErr := encryption.UnpackKeyData(loc.EncryptionKey)
			if unpackErr != nil {
				slog.WarnContext(ctx, "Key rotation: unpack failed", "key", loc.ObjectKey, "error", unpackErr)
				telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
				failed++
				continue
			}

			// Unwrap with old key, re-wrap with new key
			dek, unwrapErr := h.encryptor.Provider().UnwrapDEK(ctx, wrappedDEK, loc.KeyID)
			if unwrapErr != nil {
				slog.WarnContext(ctx, "Key rotation: unwrap failed", "key", loc.ObjectKey, "error", unwrapErr)
				telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
				failed++
				continue
			}

			newWrapped, newKeyID, wrapErr := h.encryptor.Provider().WrapDEK(ctx, dek)
			if wrapErr != nil {
				slog.WarnContext(ctx, "Key rotation: wrap failed", "key", loc.ObjectKey, "error", wrapErr)
				telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
				failed++
				continue
			}

			newKeyData := encryption.PackKeyData(baseNonce, newWrapped)
			if err := h.rawStore.UpdateEncryptionKey(ctx, loc.ObjectKey, loc.BackendName, newKeyData, newKeyID); err != nil {
				slog.WarnContext(ctx, "Key rotation: update failed", "key", loc.ObjectKey, "error", err)
				telemetry.KeyRotationObjectsTotal.WithLabelValues("error").Inc()
				failed++
				continue
			}

			telemetry.KeyRotationObjectsTotal.WithLabelValues("success").Inc()
			rotated++
		}

		// If we got fewer than batchSize, we've reached the end
		if len(locs) < batchSize {
			break
		}
	}

	slog.InfoContext(ctx, "Admin: key rotation complete", "rotated", rotated, "failed", failed, "total", total)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "complete",
		"rotated": rotated,
		"failed":  failed,
		"total":   total,
	})
}

// -------------------------------------------------------------------------
// ENCRYPT EXISTING OBJECTS
// -------------------------------------------------------------------------

// handleEncryptExisting downloads each unencrypted object from its backend,
// encrypts it, re-uploads the ciphertext, and updates the DB record. Objects
// are processed in batches to avoid holding long transactions.
func (h *Handler) handleEncryptExisting(w http.ResponseWriter, r *http.Request) {
	if h.encryptor == nil || h.rawStore == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "encryption not enabled"})
		return
	}

	ctx := r.Context()
	const batchSize = 100
	var encrypted, failed, total int

	for offset := 0; ; offset += batchSize {
		locs, err := h.rawStore.ListUnencryptedLocations(ctx, batchSize, offset)
		if err != nil {
			slog.ErrorContext(ctx, "Admin: encrypt-existing list failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list unencrypted objects"})
			return
		}
		if len(locs) == 0 {
			break
		}

		for _, loc := range locs {
			total++

			backend, err := h.manager.GetBackend(loc.BackendName)
			if err != nil {
				slog.WarnContext(ctx, "Encrypt-existing: backend not found", "key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
				telemetry.EncryptExistingObjectsTotal.WithLabelValues("error").Inc()
				failed++
				continue
			}

			// Download plaintext from backend
			result, err := backend.GetObject(ctx, loc.ObjectKey, "")
			if err != nil {
				h.manager.RecordUsage(loc.BackendName, 1, 0, 0)
				slog.WarnContext(ctx, "Encrypt-existing: download failed", "key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
				telemetry.EncryptExistingObjectsTotal.WithLabelValues("error").Inc()
				failed++
				continue
			}
			h.manager.RecordUsage(loc.BackendName, 1, loc.SizeBytes, 0)

			// Encrypt
			encResult, err := h.encryptor.Encrypt(ctx, result.Body, loc.SizeBytes)
			if err != nil {
				result.Body.Close()
				slog.WarnContext(ctx, "Encrypt-existing: encrypt failed", "key", loc.ObjectKey, "error", err)
				telemetry.EncryptExistingObjectsTotal.WithLabelValues("error").Inc()
				failed++
				continue
			}

			// Re-upload ciphertext (overwrites plaintext on backend)
			_, err = backend.PutObject(ctx, loc.ObjectKey, encResult.Body, encResult.CiphertextSize, result.ContentType, result.Metadata)
			result.Body.Close()
			if err != nil {
				h.manager.RecordUsage(loc.BackendName, 1, 0, 0)
				slog.WarnContext(ctx, "Encrypt-existing: re-upload failed", "key", loc.ObjectKey, "backend", loc.BackendName, "error", err)
				telemetry.EncryptExistingObjectsTotal.WithLabelValues("error").Inc()
				failed++
				continue
			}
			h.manager.RecordUsage(loc.BackendName, 1, 0, encResult.CiphertextSize)

			// Update DB record
			keyData := encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK)
			if err := h.rawStore.MarkObjectEncrypted(ctx, loc.ObjectKey, loc.BackendName, keyData, encResult.KeyID, loc.SizeBytes, encResult.CiphertextSize); err != nil {
				slog.WarnContext(ctx, "Encrypt-existing: DB update failed", "key", loc.ObjectKey, "error", err)
				telemetry.EncryptExistingObjectsTotal.WithLabelValues("error").Inc()
				failed++
				continue
			}

			telemetry.EncryptExistingObjectsTotal.WithLabelValues("success").Inc()
			encrypted++
		}

		if len(locs) < batchSize {
			break
		}
	}

	slog.InfoContext(ctx, "Admin: encrypt-existing complete", "encrypted", encrypted, "failed", failed, "total", total)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "complete",
		"encrypted": encrypted,
		"failed":    failed,
		"total":     total,
	})
}

// -------------------------------------------------------------------------
// INTEGRITY
// -------------------------------------------------------------------------

// codecov:ignore:start -- admin HTTP endpoints, covered by integration tests

// handleScrub triggers an on-demand scrub cycle. Accepts an optional
// batch_size query parameter (defaults to the configured scrubber batch size).
func (h *Handler) handleScrub(w http.ResponseWriter, r *http.Request) {
	icfg := h.manager.IntegrityConfig()
	if icfg == nil || !icfg.Enabled {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "skipped",
			"reason": "integrity verification is not enabled",
		})
		return
	}

	batchSize := icfg.ScrubberBatchSize
	if bs := r.URL.Query().Get("batch_size"); bs != "" {
		if v, err := strconv.ParseInt(bs, 10, 32); err == nil && v > 0 {
			batchSize = int(v)
		}
	}

	checked, failed := h.manager.Scrubber.Scrub(r.Context(), batchSize)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"checked": checked,
		"failed":  failed,
	})
}

// handleBackfillChecksums computes and stores content hashes for objects
// that don't have one. Accepts an optional batch_size query parameter
// (default 100). Paginates internally until all objects are processed or
// the request is cancelled.
func (h *Handler) handleBackfillChecksums(w http.ResponseWriter, r *http.Request) {
	icfg := h.manager.IntegrityConfig()
	if icfg == nil || !icfg.Enabled {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "skipped",
			"reason": "integrity verification is not enabled",
		})
		return
	}

	batchSize := 100
	if bs := r.URL.Query().Get("batch_size"); bs != "" {
		if v, err := strconv.ParseInt(bs, 10, 32); err == nil && v > 0 {
			batchSize = int(v)
		}
	}

	var totalProcessed int
	offset := 0
	for {
		processed, nextOffset := h.manager.Scrubber.Backfill(r.Context(), batchSize, offset)
		totalProcessed += processed
		if nextOffset == 0 {
			break
		}
		offset = nextOffset
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"processed": totalProcessed,
	})
}

// codecov:ignore:end

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("Admin: failed to encode JSON response", "error", err) //nolint:sloglint // standalone helper, no request context
	}
}
