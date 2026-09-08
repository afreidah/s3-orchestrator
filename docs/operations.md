---
description: "Day-to-day procedures: draining a backend, rebalancing copies, running integrity scrubs, managing cache, and taking trace snapshots."
title: "Operations"
linkTitle: "Operations"
weight: 30
---

Day-to-day operational procedures: drain, rebalance, scrub, cache management, and trace snapshot.


### Reloading configuration

Many settings can be updated without restarting the orchestrator by sending `SIGHUP`:

1. **Edit the config file** with your changes.
2. **Send SIGHUP** to the running process:
   ```bash
   kill -HUP $(pidof s3-orchestrator)
   ```
3. **Check the logs** to confirm the reload succeeded:
   ```
   {"level":"INFO","msg":"SIGHUP received, reloading configuration","path":"config.yaml"}
   {"level":"INFO","msg":"Configuration reload complete"}
   ```

**What takes effect immediately:**

- Log level (`server.log_level`) — can also be changed at runtime via `s3-orchestrator admin log-level -set debug`
- Bucket credentials (add/remove/rotate credentials without downtime)
- Rate limit settings (requests per second, burst)
- Backend quota limits (`quota_bytes`)
- Backend usage limits (`api_request_limit`, `egress_byte_limit`, `ingress_byte_limit`)
- Rebalance settings (strategy, interval, batch size, threshold, enable/disable)
- Replication settings (factor, worker interval, batch size, unhealthy threshold)
- Usage flush settings (interval, adaptive enabled/threshold/fast interval)

**What requires a restart:**

- `server.listen_addr`, server timeouts, `server.shutdown_delay`, `database`, `telemetry`, `ui`, `routing_strategy`, `encryption`, `redis`
- Backend structural changes (endpoint, S3 credentials, adding/removing backends)

If any of these fields change, the reload still proceeds for the reloadable settings, and warnings are logged:

```
{"level":"WARN","msg":"Config field changed but requires restart to take effect","field":"server.listen_addr"}
```

**If the config file is invalid**, the orchestrator keeps the current configuration entirely and logs the parse/validation error:

```
{"level":"ERROR","msg":"Config reload failed, keeping current config","error":"invalid config: ..."}
```

No partial reload happens — either all reloadable settings update, or none do.

### Adding a new backend

Add the backend to the `backends` list in your config and restart the orchestrator. Backend count changes are not reloadable — a restart is required. Quota limits are synced to the database on startup.

### Onboarding a client onto a new bucket

Declaring a virtual bucket and issuing the credential that reaches it are API operations, so neither needs a config edit or a `SIGHUP`. Every change takes effect on the next request: the registry the request path authenticates against is rebuilt before the command returns.

The commands below take `-addr` and `-token` explicitly. They resolve flag, then `$S3O_ADMIN_ADDR` / `$S3O_ADMIN_TOKEN`, then the config file, so an omitted target can reach an instance you did not mean.

```bash
ADDR=http://localhost:9000
TOKEN=your-admin-token

# 1. Declare the bucket.
s3-orchestrator admin -addr $ADDR -token $TOKEN bucket create -name app3-files

# 2. Declare the identity the credential will belong to. The response
#    carries the generated user_id every later command names it by.
s3-orchestrator admin -addr $ADDR -token $TOKEN user create -name app3

# 3. Mint the keypair. This response is the only place the secret appears.
s3-orchestrator admin -addr $ADDR -token $TOKEN credential issue -user <user_id> -label "app3 prod"

# 4. Grant the user the bucket.
s3-orchestrator admin -addr $ADDR -token $TOKEN grant add -user <user_id> -bucket app3-files
```

The order matters only in that a credential needs its user first and a grant needs both sides. A user created but not yet granted anything authenticates and reaches nothing, which is a safe intermediate state to leave it in.

Capture the secret at step 3. It is not stored anywhere it can be read back, and no listing renders it; a client that loses it gets a replacement keypair rather than a recovery.

Confirm what the deployment now declares, from both sources at once:

```bash
s3-orchestrator admin -addr $ADDR -token $TOKEN bucket list
s3-orchestrator admin -addr $ADDR -token $TOKEN credential list
```

Every row carries a source. An entry marked `config` comes from the config file and the API refuses to change it - edit the file and send `SIGHUP` for those. An entry marked `store` is managed through these commands.

### Removing a virtual bucket

Removal is refused while anything still depends on what is being removed, so tear down in reverse:

```bash
# Objects first - the orchestrator will not drop a namespace that still
# addresses data, which would leave those bytes occupying every backend
# they were written to with no way to reach them. Empty it with any S3
# client, or with the admin API's prefix delete.
aws --endpoint-url $S3_ENDPOINT s3 rm --recursive s3://app3-files/
curl -X DELETE -H "X-Admin-Token: $TOKEN" "$ADDR/admin/api/objects?prefix=app3-files/"

s3-orchestrator admin -addr $ADDR -token $TOKEN grant remove -user <user_id> -bucket app3-files
s3-orchestrator admin -addr $ADDR -token $TOKEN credential revoke -access-key <access_key_id>
s3-orchestrator admin -addr $ADDR -token $TOKEN user delete -id <user_id>
s3-orchestrator admin -addr $ADDR -token $TOKEN bucket delete -name app3-files
```

Each refusal names what is in the way and returns a non-zero exit code, so a script that runs these out of order stops rather than half-completing.

### Draining a backend

Draining migrates all objects off a backend to other backends without data loss. Use this when decommissioning a backend but preserving all stored objects.

1. **Start the drain:**

   ```bash
   s3-orchestrator admin drain <backend-name>
   ```

   This immediately excludes the backend from new writes (PutObject and CreateMultipartUpload skip it) and begins migrating objects in batches of 100. Any in-progress multipart uploads on the backend are aborted first.

2. **Monitor progress:**

   ```bash
   s3-orchestrator admin drain-status <backend-name>
   ```

   Returns objects remaining, bytes remaining, objects moved so far, and whether the drain is still active. Poll this periodically until `active` is `false` and `objects_remaining` is `0`.

3. **Wait for completion.** The drain runs as a background goroutine. Each object is read from the source backend, written to the least-utilized eligible backend, and the database record is atomically swapped via compare-and-swap. Failed moves are logged but don't stop the drain.

4. **Remove the backend from config and restart:**

   ```bash
   # Edit config.yaml — remove the backend entry
   # Restart or redeploy the orchestrator
   ```

   After drain completes, `DeleteBackendData` cleans up remaining database records (usage, quota, cleanup queue) automatically. Removing the backend from config on restart prevents it from being re-initialized.

**Cancelling a drain:**

```bash
s3-orchestrator admin drain-cancel <backend-name>
```

Objects already moved are not rolled back. The backend becomes eligible for new writes again.

**Metrics to watch during drain:**

| Metric | Description |
|--------|-------------|
| `s3o_drain_active` | `1` while a drain is in progress |
| `s3o_drain_objects_moved_total` | Objects successfully migrated |
| `s3o_drain_bytes_moved_total` | Bytes migrated |

> **Note:** To confirm where an object's copies live before or after a drain, open the inspector on it in `s3-orchestrator tui` or run `s3-orchestrator admin object-locations -key <key>`.

### Removing a backend

Removing deletes all database records for a backend. This is destructive — objects on that backend become inaccessible. Use `drain` first if you want to preserve data.

**Drop database records only** (objects remain on the backend's S3 storage):

```bash
s3-orchestrator admin remove-backend <backend-name>
```

**Preview what purge would destroy (dry-run):**

```bash
s3-orchestrator admin remove-backend <backend-name> --purge
```

**Drop database records AND delete S3 objects (requires confirmation):**

```bash
s3-orchestrator admin remove-backend <backend-name> --purge --confirm
```

The `--purge` flag without `--confirm` shows a preview of what would be destroyed (object count, total bytes) and exits. With `--confirm`, the CLI obtains a signed confirmation token from the server (valid for 60 seconds) and executes the purge. Individual delete failures are logged but don't stop the operation.

After removing, edit the config to remove the backend entry and restart.

> **Note:** You cannot remove a backend that is currently draining. Cancel the drain first with `drain-cancel`.

### Important: update the config after drain or remove

Drain and remove state is held in memory only — it is **not** persisted to the database. This means:

- **If the service restarts with a drained/removed backend still in the config**, `SyncQuotaLimits` re-creates the backend's quota record and the backend is re-initialized as a fresh, empty backend eligible for new writes. No data is lost, but the decommissioned backend silently starts receiving traffic again.
- **If the service crashes during an active drain**, all drain progress is lost. The backend reverts to active on restart. You would need to restart the drain.
- **SIGHUP does not remove backends** — config reload only updates quota limits and usage limits. The in-memory backend map is set at startup and cannot be modified at runtime.

**Always remove the backend from the config file and restart (or redeploy) after a drain or remove operation completes.** The dashboard UI shows a pulsing "Draining" badge on backends with an active drain so you can monitor progress visually.

### Adjusting quotas

Change `quota_bytes` in the config and send `SIGHUP`. Quota limits are synced to the database on reload. Alternatively, restart the orchestrator — `SyncQuotaLimits` also runs on startup.

### Enabling replication after initial setup

Add a `replication` section with `factor > 1` and send `SIGHUP` (or restart). When restarting, the replication worker runs immediately at startup to begin creating copies of existing objects, then continues at the configured interval. With `SIGHUP`, the new factor takes effect on the next worker tick.

Remember: the replication factor cannot exceed the number of backends.

### Enabling encryption on existing data

If you enable encryption on an orchestrator that already has unencrypted objects, those objects remain unencrypted until you explicitly encrypt them. New objects are encrypted automatically; existing ones need the `encrypt-existing` admin API.

1. **Enable encryption** in the config and restart the orchestrator.

2. **Encrypt existing objects:**

   ```bash
   s3-orchestrator admin encrypt-existing
   ```

   This processes all unencrypted objects in batches of 100: downloads from the backend, encrypts, re-uploads the ciphertext (overwriting the plaintext), and updates the database record. The response shows progress:

   ```json
   {"status": "complete", "encrypted": 1423, "failed": 0, "total": 1423}
   ```

3. **Monitor** via the `s3o_encrypt_existing_objects_total` metric (labels: `success`, `error`).

Failed objects are logged individually and can be retried by calling `encrypt-existing` again — it only processes objects without encryption metadata.

### Disabling encryption / decrypting existing data

To remove encryption from all objects and restore plaintext on backends, use the `decrypt-existing` admin API. Encryption must still be configured when you run this (the orchestrator needs the key provider to unwrap DEKs). Disable encryption in the config *after* decryption completes.

1. **Decrypt all encrypted objects:**

   ```bash
   s3-orchestrator admin decrypt-existing
   ```

   This processes all encrypted objects in batches of 100: downloads ciphertext from the backend, decrypts, re-uploads plaintext (overwriting the ciphertext), and clears encryption metadata in the database. Each object costs 2 API calls (one GET, one PUT) plus egress and ingress against the backend's usage quota. The response shows progress:

   ```json
   {"status": "complete", "decrypted": 1423, "failed": 0, "total": 1423}
   ```

2. **Disable encryption** in the config and restart the orchestrator.

3. **Monitor** via the `s3o_decrypt_existing_objects_total` metric (labels: `success`, `error`).

Failed objects are logged individually and can be retried by calling `decrypt-existing` again — it only processes objects with encryption metadata.

Both `encrypt-existing` and `decrypt-existing` keep `backend_quotas.bytes_used` consistent with the on-disk byte count: each object is rewritten at a different size (encryption inflates by per-chunk overhead, decryption removes it), and the per-backend counter advances by the size delta inside the same transaction as the metadata update. No manual reconciliation against `SUM(object_locations.size_bytes)` is needed after a run.

### Compressing existing data

Enabling compression affects new writes only, exactly as enabling encryption does. `s3-orchestrator admin compress-existing` brings what is already stored under it, and `decompress-existing` takes it back out. Both keep quota consistent the same way, moving the counter by the size difference in the same transaction as the row update. Both also take `-max=N` to convert part of a fleet and stop; the next run continues from there without anything being carried between them.

Unlike the encryption passes, these decline objects on purpose: anything below `min_size`, and anything the encoder cannot shrink past `min_ratio`, is left alone and counted as skipped. Watch `s3o_compress_existing_objects_total{status="skipped"}` alongside the success count - on media or backup data most of a fleet will be skipped, and that is the pass working. See [Compression](compression.md) for what the thresholds mean.

### Rotating encryption keys

Key rotation re-wraps DEKs with a new master key without re-encrypting object data. This is a metadata-only operation and is fast regardless of object sizes.

1. **Generate a new master key:**

   ```bash
   openssl rand -base64 32
   ```

2. **Update the config** — set the new key as `master_key` and move the old key to `previous_keys`:

   ```yaml
   encryption:
     enabled: true
     master_key: "${NEW_ENCRYPTION_KEY}"
     previous_keys:
       - "${OLD_ENCRYPTION_KEY}"
   ```

3. **Restart the orchestrator** (encryption config is not reloadable).

4. **Re-wrap all DEKs:**

   ```bash
   s3-orchestrator admin rotate-encryption-key --old-key-id config-0
   ```

   The `old-key-id` identifies which key's DEKs to re-wrap. For inline config keys, the ID is `config-0` for the primary and `config-1`, `config-2`, etc. for previous keys in order. For file-based keys, the ID is `file-0`. For Vault Transit, it's the key name.

   The response shows progress:

   ```json
   {"status": "complete", "rotated": 1423, "failed": 0, "total": 1423}
   ```

5. **After all DEKs are re-wrapped**, you can optionally remove the old key from `previous_keys` and restart. Objects that were rotated no longer need the old key.

**Metrics to watch during rotation:**

| Metric | Description |
|--------|-------------|
| `s3o_key_rotation_objects_total{status="success"}` | DEKs successfully re-wrapped |
| `s3o_key_rotation_objects_total{status="error"}` | DEKs that failed re-wrapping |

### Rotating client credentials

How you rotate depends on which source declared the credential. `credential list` marks each one `config` or `store`.

**A stored credential** rotates through the API, with no file edit, no `SIGHUP` and no coordination window. Several keypairs may belong to one user, so the replacement is live before the old one is withdrawn:

```bash
# 1. Issue the replacement. Both keypairs now authenticate.
s3-orchestrator admin -addr $ADDR -token $TOKEN credential issue -user <user_id> -label "app1 rotated 2026-09"

# 2. Update the client to use the new keypair.

# 3. Revoke the old one. It stops authenticating on the next request.
s3-orchestrator admin -addr $ADDR -token $TOKEN credential revoke -access-key <old_access_key_id>
```

Step 3 takes effect immediately rather than at the next reload, so a leaked key is withdrawn the moment the command returns. Revoking one keypair leaves its siblings working, which is what makes the overlap in step 1 safe.

**A config-declared credential** is edited in the file, as below.

**Example: rotating config credentials without downtime**

Update the credentials in the bucket config and send `SIGHUP`. The new credentials take effect immediately and old credentials stop working. Coordinate with the tenant to update their client configuration at the same time.

To perform a zero-downtime rotation, temporarily declare both old and new credentials:

1. Add the new credential alongside the old one:
   ```yaml
   buckets:
     - name: "app1-files"
       credentials:
         - access_key_id: "OLD_KEY"
           secret_access_key: "old_secret"
         - access_key_id: "NEW_KEY"
           secret_access_key: "new_secret"
   ```
2. Send `SIGHUP` — both credentials now work.
3. Update the client to use the new credentials.
4. Remove the old credential from the config and send `SIGHUP` again.

