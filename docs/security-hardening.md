---
title: "S3-Orchestrator Security Hardening"
linkTitle: "Security Hardening"
weight: 5
---

<div style="text-align: center; margin-bottom: 1.5rem;">
  <img src="/images/logo.png" alt="s3-orchestrator" style="max-width: 200px; height: auto;">
</div>

This guide covers recommended security practices for production deployments of the S3 Orchestrator.

## TLS Configuration

### Basic TLS

Enable TLS by providing a certificate and private key:

```yaml
server:
  tls:
    cert_file: "/etc/s3-orchestrator/tls/server.crt"
    key_file: "/etc/s3-orchestrator/tls/server.key"
    min_version: "1.2"   # "1.2" (default) or "1.3"
```

- Use `min_version: "1.3"` for environments where all clients support TLS 1.3.
- Use `min_version: "1.2"` (default) for broader compatibility. TLS 1.0 and 1.1 are never supported.
- Certificates are reloaded automatically on `SIGHUP` without dropping connections.

### Certificate Renewal

The orchestrator watches for `SIGHUP` to reload certificates from disk. Integrate with your certificate manager:

```bash
# After certificate renewal (e.g., certbot, vault-cert-manager)
systemctl reload s3-orchestrator
```

## Mutual TLS (mTLS)

mTLS requires clients to present a certificate signed by a trusted CA. This restricts access to authorized clients only.

### Setup

1. **Generate a CA** (or use an existing one):

   ```bash
   openssl genrsa -out ca.key 4096
   openssl req -new -x509 -key ca.key -out ca.crt -days 3650 \
     -subj "/CN=S3 Orchestrator CA"
   ```

2. **Generate a client certificate**:

   ```bash
   openssl genrsa -out client.key 2048
   openssl req -new -key client.key -out client.csr \
     -subj "/CN=my-app"
   openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key \
     -CAcreateserial -out client.crt -days 365
   ```

3. **Configure the orchestrator**:

   ```yaml
   server:
     tls:
       cert_file: "/etc/s3-orchestrator/tls/server.crt"
       key_file: "/etc/s3-orchestrator/tls/server.key"
       client_ca_file: "/etc/s3-orchestrator/tls/ca.crt"
   ```

4. **Test with curl**:

   ```bash
   curl --cert client.crt --key client.key \
     --cacert server-ca.crt \
     https://s3-orchestrator:9000/health
   ```

Clients without a valid certificate receive a TLS handshake error and cannot connect.

## Server-Side Encryption

When encryption is enabled, all objects are encrypted with AES-256-GCM before being stored on backends. Backends never see plaintext — they only store ciphertext. This protects against data exposure if a backend is compromised or if storage media is improperly decommissioned.

### Key Management

The master key wraps per-object DEKs using envelope encryption. Choose the key source based on your security requirements:

| Source | Security level | Use case |
|--------|---------------|----------|
| `master_key` (inline/env var) | Good | Dev, staging, simple deployments with env var injection |
| `master_key_file` | Better | Bare-metal with config management (Ansible, Puppet) provisioning the key file |
| Vault Transit | Best | Production with HSM-backed key management, audit logging, automatic key versioning |

**Recommendations:**

- **Never commit encryption keys** to version control. Use `${ENV_VAR}` expansion or `master_key_file`.
- **Restrict key file permissions:** `chmod 600 /path/to/keyfile && chown root:root /path/to/keyfile`
- **Rotate keys periodically** using the `rotate-encryption-key` admin API. See the [Admin Guide](admin-guide.md#rotating-encryption-keys).
- **Keep previous keys** in the config until all DEKs have been re-wrapped. Removing an old key before rotation completes makes objects encrypted with that key unrecoverable.
- **Back up your encryption keys** separately from your data backups. Without the key, encrypted data is unrecoverable.

### Vault Transit Integration

For production deployments, Vault Transit provides the strongest key management:

```yaml
encryption:
  enabled: true
  vault:
    address: "https://vault.example.com:8200"
    token: "${VAULT_TOKEN}"
    key_name: "s3-orchestrator"
    mount_path: "transit"
```

- The orchestrator calls Vault to wrap/unwrap DEKs — the master key never leaves Vault.
- Vault provides audit logging of all key operations.
- Key rotation in Vault automatically versions the key; the orchestrator's `rotate-encryption-key` API re-wraps DEKs to the latest version.

### Encryption Metrics

Monitor encryption health with these Prometheus metrics:

| Metric | What to watch |
|--------|---------------|
| `s3proxy_encryption_errors_total` | Any non-zero rate indicates encryption/decryption failures |
| `s3proxy_encrypt_existing_objects_total{status="error"}` | Failures during bulk encryption of existing data |
| `s3proxy_key_rotation_objects_total{status="error"}` | Failures during key rotation |

## Configuration File Security

The config file contains sensitive credentials:

- Database password (`database.password`)
- Backend S3 credentials (`backends[].access_key_id`, `backends[].secret_access_key`)
- UI admin credentials (`ui.admin_key`, `ui.admin_secret`, `ui.admin_token`)
- Client S3 credentials (`buckets[].credentials[]`)
- Encryption master key (`encryption.master_key`, `encryption.previous_keys[]`)
- Vault token (`encryption.vault.token`)

### Recommendations

**File permissions:**

```bash
chmod 600 /etc/s3-orchestrator/config.yaml
chown root:root /etc/s3-orchestrator/config.yaml
```

**Use environment variable expansion** to avoid storing secrets in the file:

```yaml
database:
  password: "${DB_PASSWORD}"

backends:
  - access_key_id: "${OCI_ACCESS_KEY}"
    secret_access_key: "${OCI_SECRET_KEY}"
```

Provide the environment variables via systemd `EnvironmentFile`, Vault agent injection, Nomad template blocks, or Kubernetes secrets.

**Never commit config files** with real credentials to version control. The `.gitignore` already excludes `/config.yaml` at the project root.

## Network Segmentation

- **PostgreSQL** should only be reachable from orchestrator instances. It does not need public access.
- **Storage backends** (if self-hosted like MinIO) should only be reachable from orchestrator instances.
- **The orchestrator** is the only component that needs to be exposed to clients.
- **Admin API** (`/admin/api/`) is protected by token auth and per-IP rate limiting (when enabled). Consider additionally restricting access at the network level (firewall rules or reverse proxy ACLs) for defense in depth.

### Kubernetes Hardening

The provided Kubernetes manifests include several security measures:

- **seccompProfile: RuntimeDefault** — applies the default seccomp profile to restrict syscalls
- **automountServiceAccountToken: false** — the orchestrator does not need Kubernetes API access
- **NetworkPolicy** — restricts ingress to port 9000 only; egress is permissive since backend endpoints are config-driven
- **readOnlyRootFilesystem**, **runAsNonRoot**, **capabilities.drop: ALL** — standard container hardening (see `deploy/kubernetes/deployment.yaml`)

```
Internet --> Reverse Proxy --> S3 Orchestrator --> PostgreSQL (private)
                                              --> Backends (private)
```

## Audit Logging

The orchestrator emits structured audit log entries with `"audit":true` for security-relevant operations:

- Every S3 request (GET, PUT, DELETE, etc.)
- Storage-level operations (backend reads, writes, deletes)
- Background operations (rebalance, replication, cleanup)

### Request ID Correlation

Each request gets a unique ID that flows through all log entries:

- Clients can send `X-Request-Id` header (honored if present)
- Otherwise, a 16-byte hex ID is generated automatically
- Returned as `X-Amz-Request-Id` in the response
- Propagated to storage operations for end-to-end tracing

### Monitoring Patterns

Filter audit events in your log aggregator:

```
# All audit events
jq 'select(.audit == true)'

# All write operations
jq 'select(.audit == true and .event == "s3.PutObject")'

# Events for a specific request
jq 'select(.request_id == "abc123...")'
```

The `s3proxy_audit_events_total` Prometheus counter with `event` label tracks audit event volume for alerting.

## Admission Control

Limit the total number of concurrent S3 requests to prevent backend and database saturation under load:

```yaml
server:
  max_concurrent_requests: 30    # 0 = unlimited (default)
```

When the limit is reached, new requests receive `503 SlowDown` immediately instead of queueing and consuming resources. A good starting point is 2-3x your `database.max_conns` value, since every S3 operation requires at least one database query. Monitor `s3proxy_admission_rejections_total` and `s3proxy_inflight_requests` to tune the value.

## Rate Limiting

Protect against abuse and accidental overload:

```yaml
rate_limit:
  enabled: true
  requests_per_sec: 100   # token refill rate
  burst: 200              # max burst size
  cleanup_interval: "1m"  # eviction sweep interval (default: 1m)
  cleanup_max_age: "5m"   # evict entries not seen within this window (default: 5m)
```

A background goroutine evicts per-IP entries not seen within `cleanup_max_age` every `cleanup_interval`. Under sustained attack with high source-IP cardinality, the map can hold up to `cleanup_max_age` worth of unique IPs. Lower both values for tighter memory bounds.

### Behind a Reverse Proxy

When the orchestrator sits behind a load balancer, configure trusted proxies so rate limiting uses the real client IP from `X-Forwarded-For`:

```yaml
rate_limit:
  enabled: true
  requests_per_sec: 100
  burst: 200
  trusted_proxies:
    - "10.0.0.0/8"
    - "172.16.0.0/12"
```

Without this, all requests appear to come from the proxy IP and share a single rate limit bucket.

The login throttle (brute-force protection on the dashboard login) also uses the same `trusted_proxies` configuration and IP extraction logic, so it correctly identifies real client IPs behind a reverse proxy.

## Request Body Limits

All admin and UI JSON endpoints enforce a 1 MB request body limit via `http.MaxBytesReader`. This prevents memory exhaustion from oversized payloads. File uploads use the configured `max_object_size` limit instead. These limits are built-in and not user-configurable.

## Web UI Authentication

### Admin Token Separation

By default, the admin API (`/admin/api/`) uses the same `admin_key` as the dashboard login. For production deployments, set a separate `admin_token` so the dashboard login credential and the API token can be managed independently:

```yaml
ui:
  admin_key: "dashboard-login-key"
  admin_secret: "dashboard-login-secret"
  admin_token: "separate-api-token"    # falls back to admin_key if not set
```

### Secure Cookies Behind TLS Proxies

When the orchestrator sits behind a TLS-terminating reverse proxy (Traefik, nginx, ALB), the connection to the container is plaintext HTTP, so `r.TLS` is nil and the `Secure` flag is not set on session cookies by default. Set `force_secure_cookies` to always set the `Secure` flag:

```yaml
ui:
  force_secure_cookies: true
```

This ensures browsers only send the session cookie over HTTPS, even though the orchestrator itself sees HTTP.

### Bcrypt-Hashed Admin Secret

For bare-metal deployments where the config file is stored on disk without external secret injection, use a bcrypt hash for `admin_secret` instead of plaintext:

```bash
# Generate a bcrypt hash
htpasswd -nbBC 10 "" 'your-secret' | cut -d: -f2
```

```yaml
ui:
  enabled: true
  admin_key: "ADMIN_ACCESS_KEY"
  admin_secret: "$2y$10$..."   # bcrypt hash
```

The orchestrator detects bcrypt hashes automatically (any value starting with `$2`). Plaintext secrets continue to work — no migration is required.

**Recommendation:** Use bcrypt for bare-metal and `.deb` installations. For container deployments with Vault, Nomad templates, or Kubernetes secrets, plaintext with `${ENV_VAR}` expansion is equally secure since the secret never touches disk.

### Session Portability

Session keys are derived deterministically from the config (via HMAC-SHA256), so sessions survive restarts and are portable across instances sharing the same config. No session storage or shared state is required beyond the config file itself.

For multi-instance deployments behind a load balancer, ensure all instances use the same `admin_secret` (or the same `session_secret` if set). A session created on one instance will be accepted by any other instance with matching config.

## Credential Rotation

S3 client credentials can be rotated without downtime using the SIGHUP reload mechanism. See the [admin guide](admin-guide.md#rotating-client-credentials) for the zero-downtime rotation procedure.

The admin API token (`ui.admin_token`, or `ui.admin_key` if `admin_token` is not set) requires a restart to change since the UI config section is not reloadable.
