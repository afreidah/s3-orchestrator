---
title: "Deploying on Nomad with Vault"
weight: 6
---


This guide walks through deploying S3 Orchestrator as a HashiCorp Nomad job with secrets managed by HashiCorp Vault. Nomad's template stanza renders the configuration file at deploy time, pulling credentials from Vault so that no secrets are stored in the job definition or checked into version control.

## Overview

The deployment uses three HashiCorp components:

- **Nomad** schedules and runs the orchestrator container
- **Vault** stores all secrets (database credentials, backend access keys, UI credentials) and provides Transit encryption keys
- **Consul** provides service discovery so the orchestrator can find PostgreSQL, Vault, and Tempo by DNS name

The orchestrator runs as a Docker container on Nomad. At startup, Nomad's template stanza fetches secrets from Vault's KV store and renders a complete `config.yaml` into the task's secrets directory. The container reads this file and never sees Vault directly (except for Transit encryption, which uses a Vault token for ongoing key operations).

## Prerequisites

- A running Nomad cluster with Docker driver enabled
- Vault integrated with Nomad ([Nomad-Vault integration docs](https://developer.hashicorp.com/nomad/docs/integrations/vault-integration))
- Consul for service discovery (optional but recommended)
- PostgreSQL accessible from Nomad clients
- S3 Orchestrator Docker image pushed to a registry your Nomad clients can pull from
- One or more S3-compatible storage backends with credentials

## Step 1: Store Secrets in Vault

Create a KV v2 secret at `secret/data/s3-orchestrator` containing all the credentials the orchestrator needs:

```bash
vault kv put secret/s3-orchestrator \
  access_key="YOUR_BUCKET_ACCESS_KEY" \
  secret_key="YOUR_BUCKET_SECRET_KEY" \
  db_username="s3_orchestrator" \
  db_password="DB_PASSWORD" \
  oci_s3_endpoint="https://namespace.compat.objectstorage.us-ashburn-1.oraclecloud.com" \
  oci_s3_region="us-ashburn-1" \
  oci_s3_bucket="my-bucket" \
  oci_s3_access_key="OCI_ACCESS_KEY" \
  oci_s3_secret_key="OCI_SECRET_KEY" \
  r2_s3_endpoint="https://account-id.r2.cloudflarestorage.com" \
  r2_s3_bucket="my-bucket" \
  r2_s3_access_key="R2_ACCESS_KEY" \
  r2_s3_secret_key="R2_SECRET_KEY" \
  ui_admin_key="ADMIN_KEY" \
  ui_admin_secret="ADMIN_SECRET" \
  ui_session_secret="$(openssl rand -hex 32)"
```

{{% notice tip %}}
Generate strong credentials for the virtual bucket and UI admin with `openssl rand -hex 20` for keys and `openssl rand -base64 30` for secrets.
{{% /notice %}}

## Step 2: Create a Vault Policy and Role

Create a Vault policy that grants the orchestrator read access to its secrets and, if using Vault Transit encryption, access to the transit engine:

```hcl
# s3-orchestrator-policy.hcl
path "secret/data/s3-orchestrator" {
  capabilities = ["read"]
}

path "transit/encrypt/s3-orchestrator" {
  capabilities = ["update"]
}

path "transit/decrypt/s3-orchestrator" {
  capabilities = ["update"]
}

path "transit/keys/s3-orchestrator" {
  capabilities = ["read"]
}

path "pki_int/cert/ca" {
  capabilities = ["read"]
}
```

Apply the policy and create a role for Nomad workload identity:

```bash
vault policy write s3-orchestrator s3-orchestrator-policy.hcl

vault write auth/jwt-nomad/role/s3-orchestrator \
  role_type="jwt" \
  bound_audiences="vault.io" \
  user_claim="/nomad_job_id" \
  user_claim_json_pointer=true \
  claim_mappings='{"nomad_namespace":"nomad_namespace","nomad_job_id":"nomad_job_id"}' \
  token_type="service" \
  token_policies="s3-orchestrator" \
  token_period="768h" \
  token_ttl="768h"
```

{{% notice info %}}
This example uses Nomad's [workload identity](https://developer.hashicorp.com/nomad/docs/concepts/workload-identity) with JWT auth. If your cluster uses the legacy token-based integration, create a token role instead and reference it in the Nomad server config.
{{% /notice %}}

## Step 3: Set Up Vault Transit (Optional)

If you want server-side encryption with Vault-managed keys:

```bash
vault secrets enable transit
vault write -f transit/keys/s3-orchestrator type=aes256-gcm96
```

The orchestrator will use this key for envelope encryption — each object gets a unique data encryption key (DEK) that is wrapped by the Vault transit key. The encrypted DEK is stored alongside the object, so Vault is only called during encrypt/decrypt operations, not for every byte of data.

## Step 4: Write the Nomad Job

Here is a complete Nomad job specification. The `template` stanza fetches secrets from Vault and renders `config.yaml`:

```hcl
job "s3-orchestrator" {
  region      = "global"
  datacenters = ["dc1"]
  type        = "service"

  update {
    max_parallel      = 1
    health_check      = "checks"
    min_healthy_time  = "30s"
    healthy_deadline  = "5m"
    progress_deadline = "10m"
    auto_revert       = true
  }

  group "s3-orchestrator" {
    count = 1

    network {
      mode = "host"
      port "http" {
        static = 9000
      }
    }

    restart {
      attempts = 3
      interval = "5m"
      delay    = "15s"
      mode     = "fail"
    }

    service {
      name     = "s3-orchestrator"
      port     = "http"
      provider = "consul"

      # Liveness — always 200, keeps the allocation alive during DB outages.
      check {
        type     = "http"
        path     = "/health"
        interval = "10s"
        timeout  = "3s"
      }

      # Readiness — returns 503 until startup completes and during shutdown
      # drain. Gates rolling deploys so traffic only routes to ready instances.
      check {
        type      = "http"
        path      = "/health/ready"
        interval  = "5s"
        timeout   = "2s"
        on_update = "require_healthy"
      }
    }

    task "s3-orchestrator" {
      driver = "docker"

      vault {
        role = "s3-orchestrator"
      }

      identity {
        env  = true
        file = true
        aud  = ["vault.io"]
      }

      config {
        image        = "registry.example.com/s3-orchestrator:v0.13.0"
        ports        = ["http"]
        network_mode = "host"
        args         = ["-config", "/secrets/config.yaml"]
        volumes      = [
          "secrets/config.yaml:/secrets/config.yaml:ro",
          "secrets/vault-ca.pem:/secrets/vault-ca.pem:ro",
        ]
      }

      # --- Configuration template (rendered from Vault secrets) ---
      template {
        data = <<EOH
{{ with secret "secret/data/s3-orchestrator" }}
server:
  listen_addr: "0.0.0.0:9000"
  max_object_size: 5368709120    # 5 GB — reject uploads larger than this
  backend_timeout: "5m"
  shutdown_delay: "5s"

routing_strategy: "spread"

buckets:
  - name: "default"
    credentials:
      - access_key_id: "{{ .Data.data.access_key }}"
        secret_access_key: "{{ .Data.data.secret_key }}"

database:
  host: "postgres.service.consul"
  port: 5432
  database: "s3_orchestrator"
  user: "{{ .Data.data.db_username }}"
  password: "{{ .Data.data.db_password }}"
  ssl_mode: "require"
  max_conns: 50
  min_conns: 10
  max_conn_lifetime: "5m"

backends:
  - name: "oci"
    endpoint: "{{ .Data.data.oci_s3_endpoint }}"
    region: "{{ .Data.data.oci_s3_region }}"
    bucket: "{{ .Data.data.oci_s3_bucket }}"
    access_key_id: "{{ .Data.data.oci_s3_access_key }}"
    secret_access_key: "{{ .Data.data.oci_s3_secret_key }}"
    force_path_style: true
    quota_bytes: 10737418240       # 10 GB
    api_request_limit: 50000
    egress_byte_limit: 10737418240 # 10 GB
  - name: "r2"
    endpoint: "{{ .Data.data.r2_s3_endpoint }}"
    region: "auto"
    bucket: "{{ .Data.data.r2_s3_bucket }}"
    access_key_id: "{{ .Data.data.r2_s3_access_key }}"
    secret_access_key: "{{ .Data.data.r2_s3_secret_key }}"
    force_path_style: true
    quota_bytes: 10737418240       # 10 GB
    api_request_limit: 1000000

replication:
  factor: 2
  worker_interval: "5m"
  batch_size: 50
  unhealthy_threshold: "10m"

encryption:
  enabled: true
  vault:
    address: "https://vault.service.consul:8200"
    token: "${VAULT_TOKEN}"
    key_name: "s3-orchestrator"
    mount_path: "transit"
    ca_cert: "/secrets/vault-ca.pem"

circuit_breaker:
  failure_threshold: 3
  open_timeout: "15s"
  cache_ttl: "60s"

backend_circuit_breaker:
  enabled: true
  failure_threshold: 5
  open_timeout: "5m"

rate_limit:
  enabled: true
  requests_per_sec: 100
  burst: 200
  trusted_proxies:
    - "10.0.0.0/8"
    - "172.16.0.0/12"
    - "192.168.0.0/16"
    - "127.0.0.1/32"

ui:
  enabled: true
  admin_key: "{{ .Data.data.ui_admin_key }}"
  admin_secret: "{{ .Data.data.ui_admin_secret }}"
  session_secret: "{{ .Data.data.ui_session_secret }}"
  force_secure_cookies: true

usage_flush:
  interval: "30s"
  adaptive_enabled: true
  adaptive_threshold: 0.8
  fast_interval: "5s"

telemetry:
  metrics:
    enabled: true
    path: "/metrics"
  tracing:
    enabled: true
    endpoint: "tempo.service.consul:4317"
    insecure: true
    sample_rate: 0.1             # 10% of requests traced; use 1.0 for debugging
{{ end }}
EOH
        destination = "secrets/config.yaml"
        perms       = "0400"
        change_mode = "restart"
      }

      # --- Vault CA certificate (for Transit TLS) ---
      template {
        data        = <<EOH
{{ with secret "pki_int/cert/ca" }}{{ .Data.certificate }}{{ end }}
EOH
        destination = "secrets/vault-ca.pem"
        change_mode = "restart"
      }

      resources {
        cpu    = 500
        memory = 512
      }

      kill_timeout = "30s"
      kill_signal  = "SIGTERM"
    }
  }
}
```

### Key Points in the Job Spec

- **`vault` stanza** tells Nomad to obtain a Vault token for this task using the `s3-orchestrator` role
- **`identity` stanza** enables workload identity — Nomad presents a signed JWT to Vault's JWT auth method
- **`template` stanza** with `{{ with secret "secret/data/s3-orchestrator" }}` fetches the KV secret at deploy time and interpolates values into `config.yaml`
- **`perms = "0400"`** restricts the rendered config to read-only — secrets are only readable by the task process
- **`change_mode = "restart"`** means the task restarts automatically if the Vault secret is updated or the lease is renewed with new values
- **`${VAULT_TOKEN}`** in the encryption config is the Nomad-issued Vault token, injected as an environment variable — the orchestrator uses it for ongoing Transit API calls
- **Volumes** bind-mount the rendered secrets from Nomad's secrets directory into the container at `/secrets/`
- **Two health checks** — `/health` (liveness) keeps the allocation alive during DB outages, `/health/ready` (readiness) with `on_update = "require_healthy"` gates rolling deploys
- **`trusted_proxies`** ensures rate limiting uses the real client IP when behind a load balancer, not the proxy's IP
- **`backend_circuit_breaker`** isolates individual backend failures so a single provider outage doesn't degrade the entire service
- **`replication.factor: 2`** ensures every object exists on at least two backends for redundancy
- **`usage_flush` adaptive mode** shortens the flush interval as backends approach usage limits, improving enforcement accuracy

## Step 5: Deploy

```bash
nomad job run s3-orchestrator.nomad.hcl
```

Monitor the deployment:

```bash
nomad job status s3-orchestrator
nomad alloc logs -f <alloc-id>
```

The orchestrator runs database migrations automatically on startup. Once the health check passes, the service registers in Consul and is ready to accept requests.

## Step 6: Verify

Check that the orchestrator is healthy:

```bash
curl http://localhost:9000/health
```

Configure the AWS CLI with the virtual bucket credentials you stored in Vault (the `access_key` and `secret_key` values, not the backend credentials):

```bash
export AWS_ACCESS_KEY_ID="YOUR_BUCKET_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="YOUR_BUCKET_SECRET_KEY"
export AWS_ENDPOINT_URL="http://localhost:9000"
export AWS_DEFAULT_REGION="us-east-1"

# Upload a test object
aws s3 cp testfile.txt s3://default/testfile.txt

# List objects
aws s3 ls s3://default/

# Download it back
aws s3 cp s3://default/testfile.txt downloaded.txt
```

## Adding a Reverse Proxy

In production you'll typically front the orchestrator with a reverse proxy for TLS termination. If you're running Traefik on Nomad with Consul Catalog, add service tags to the job:

```hcl
service {
  # ... existing config ...
  tags = [
    "traefik.enable=true",
    "traefik.http.routers.s3-orchestrator.rule=Host(`s3.example.com`)",
    "traefik.http.routers.s3-orchestrator.entrypoints=websecure",
    "traefik.http.routers.s3-orchestrator.tls=true",
  ]
}
```

Traefik discovers the service via Consul and routes traffic automatically.

## Secret Rotation

To rotate any credential:

1. Update the secret in Vault: `vault kv put secret/s3-orchestrator ...`
2. Nomad detects the change on the next template poll (default: 5 minutes)
3. The task restarts with the new configuration

For immediate rotation, restart the allocation manually:

```bash
nomad alloc restart <alloc-id>
```

## Scaling

To run multiple instances, increase the `count` in the group stanza:

```hcl
group "s3-orchestrator" {
  count = 3
  # ...
}
```

Multiple instances are safe to run concurrently. PostgreSQL advisory locks ensure only one instance runs each background worker (rebalancer, replicator, cleanup queue). All instances serve API requests.

## Important Notes

- The `secrets/` directory in Nomad is tmpfs-backed and never written to disk — credentials exist only in memory
- Vault token renewal is handled automatically by Nomad — the orchestrator does not need to manage token lifecycle
- If Vault is temporarily unavailable, the existing rendered config remains in place and the orchestrator continues running, but Transit encryption operations will fail until Vault recovers
- The `auto_revert` update strategy rolls back to the previous version if the new deployment fails health checks
- Pin your Docker image to a specific version tag (e.g., `v0.13.0`) rather than `latest` to ensure reproducible deployments
