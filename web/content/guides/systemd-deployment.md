---
title: "Deploying with systemd"
weight: 7
---


This guide walks through installing S3 Orchestrator as a systemd service on a bare-metal or virtual machine using the Debian package. The package ships a systemd unit, sample configuration, and maintainer scripts that create a dedicated system user with filesystem hardening out of the box.

## Overview

The Debian package installs:

- `/usr/bin/s3-orchestrator` — the statically linked binary
- `/etc/s3-orchestrator/config.yaml` — sample configuration (preserved on upgrade)
- `/etc/default/s3-orchestrator` — environment variable overrides for secrets
- `/usr/lib/systemd/system/s3-orchestrator.service` — systemd unit with security hardening
- `/var/lib/s3-orchestrator/` — working directory for the service user

The package maintainer scripts create a dedicated `s3-orchestrator` system user and group, enable the service, but do not start it — giving you a chance to configure everything first.

## Prerequisites

- Debian or Ubuntu-based Linux distribution
- PostgreSQL accessible from the host
- One or more S3-compatible storage backends with credentials
- Root or sudo access for package installation

## Step 1: Install the Package

Download the `.deb` from the [GitHub Releases](https://github.com/afreidah/s3-orchestrator/releases) page:

```bash
# For amd64
curl -LO https://github.com/afreidah/s3-orchestrator/releases/latest/download/s3-orchestrator_amd64.deb

# For arm64
curl -LO https://github.com/afreidah/s3-orchestrator/releases/latest/download/s3-orchestrator_arm64.deb

sudo dpkg -i s3-orchestrator_*.deb
```

{{% notice tip %}}
You can also build the package from source with `make deb`. This requires Go and [GoReleaser](https://goreleaser.com/) installed locally.
{{% /notice %}}

After installation, the service is enabled but not yet running:

```bash
systemctl status s3-orchestrator
# ● s3-orchestrator.service - S3 Orchestrator - Unified S3 Storage Endpoint
#      Loaded: loaded (/usr/lib/systemd/system/s3-orchestrator.service; enabled)
#      Active: inactive (dead)
```

## Step 2: Set Credentials

Secrets are stored in `/etc/default/s3-orchestrator` and injected into the config via `${VAR}` expansion. Edit the file and uncomment the variables you need:

```bash
sudo editor /etc/default/s3-orchestrator
```

```bash
# --- Database credentials ---
DB_HOST=localhost
DB_PORT=5432
DB_NAME=s3_orchestrator
DB_USER=s3_orchestrator
DB_PASSWORD=your-database-password
DB_SSL_MODE=require

# --- Backend credentials ---
BACKEND1_NAME=oci
BACKEND1_ENDPOINT=https://namespace.compat.objectstorage.us-ashburn-1.oraclecloud.com
BACKEND1_REGION=us-ashburn-1
BACKEND1_BUCKET=my-bucket
BACKEND1_ACCESS_KEY=your-oci-access-key
BACKEND1_SECRET_KEY=your-oci-secret-key

# --- Bucket client credentials ---
BUCKET_ACCESS_KEY=your-client-access-key
BUCKET_SECRET_KEY=your-client-secret-key
```

The file is installed with `0640` permissions (root-owned, readable by the `s3-orchestrator` group) so credentials are not world-readable.

{{% notice tip %}}
Generate strong credentials with `openssl rand -hex 20` for access keys and `openssl rand -base64 30` for secret keys.
{{% /notice %}}

## Step 3: Configure the Service

Edit the main configuration file:

```bash
sudo editor /etc/s3-orchestrator/config.yaml
```

The sample config ships with all options documented and commented. Here is a production-ready example:

```yaml
server:
  listen_addr: ":9000"
  max_object_size: 5368709120    # 5 GB
  backend_timeout: "2m"
  shutdown_delay: "5s"

database:
  host: ${DB_HOST}
  port: ${DB_PORT}
  database: ${DB_NAME}
  user: ${DB_USER}
  password: ${DB_PASSWORD}
  ssl_mode: ${DB_SSL_MODE}
  max_conns: 10
  min_conns: 5
  max_conn_lifetime: "5m"

buckets:
  - name: default
    credentials:
      - access_key_id: ${BUCKET_ACCESS_KEY}
        secret_access_key: ${BUCKET_SECRET_KEY}

backends:
  - name: ${BACKEND1_NAME}
    endpoint: ${BACKEND1_ENDPOINT}
    region: ${BACKEND1_REGION}
    bucket: ${BACKEND1_BUCKET}
    access_key_id: ${BACKEND1_ACCESS_KEY}
    secret_access_key: ${BACKEND1_SECRET_KEY}
    force_path_style: true
    quota_bytes: 10737418240       # 10 GB
    api_request_limit: 50000
    egress_byte_limit: 10737418240 # 10 GB

routing_strategy: "spread"

replication:
  factor: 2
  worker_interval: "5m"
  batch_size: 50

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
  admin_key: ${UI_ADMIN_KEY}
  admin_secret: ${UI_ADMIN_SECRET}
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
```

Add more backends by defining `BACKEND2_*` variables in `/etc/default/s3-orchestrator` and adding corresponding entries to the `backends` list.

## Step 4: Prepare the Database

Create the PostgreSQL database and user:

```bash
sudo -u postgres psql <<SQL
CREATE USER s3_orchestrator WITH PASSWORD 'your-database-password';
CREATE DATABASE s3_orchestrator OWNER s3_orchestrator;
SQL
```

Database migrations are embedded in the binary and applied automatically on startup — no manual migration step is needed.

## Step 5: Start the Service

```bash
sudo systemctl start s3-orchestrator
```

Check the logs:

```bash
journalctl -u s3-orchestrator -f
```

You should see the startup sequence: database migrations, backend connectivity checks, and the listening address.

## Step 6: Verify

```bash
# Check the health endpoint
curl http://localhost:9000/health

# Configure credentials and test
export AWS_ACCESS_KEY_ID="your-client-access-key"
export AWS_SECRET_ACCESS_KEY="your-client-secret-key"
export AWS_ENDPOINT_URL="http://localhost:9000"
export AWS_DEFAULT_REGION="us-east-1"

# Upload a test object
aws s3 cp testfile.txt s3://default/testfile.txt

# List objects
aws s3 ls s3://default/

# Download it back
aws s3 cp s3://default/testfile.txt downloaded.txt
```

## Systemd Security Hardening

The shipped systemd unit includes several security features:

```ini
User=s3-orchestrator          # Runs as a dedicated non-root user
Group=s3-orchestrator
ProtectSystem=strict          # Read-only access to / and /usr
ProtectHome=yes               # No access to /home, /root, /run/user
NoNewPrivileges=yes           # Cannot escalate privileges
ReadWritePaths=/var/lib/s3-orchestrator  # Only writable path
LimitNOFILE=65536             # Raised file descriptor limit
```

The service runs with the minimum permissions needed. The config directory `/etc/s3-orchestrator` is owned by `root:s3-orchestrator` with mode `0750`, so the service can read but not modify its own configuration.

## Configuration Reload

The orchestrator supports hot configuration reload via `SIGHUP`. When you change `config.yaml`, reload without restarting:

```bash
sudo systemctl reload s3-orchestrator
```

This re-reads the configuration file and applies changes to rate limits, routing strategy, backend settings, and UI options without dropping active connections. TLS certificates are also hot-reloaded if configured.

## Adding TLS

For direct TLS termination (without a reverse proxy), add TLS configuration to `config.yaml`:

```yaml
server:
  tls:
    cert_file: "/etc/s3-orchestrator/tls/cert.pem"
    key_file: "/etc/s3-orchestrator/tls/key.pem"
    min_version: "1.2"
```

Set up the certificate files:

```bash
sudo mkdir -p /etc/s3-orchestrator/tls
sudo cp cert.pem key.pem /etc/s3-orchestrator/tls/
sudo chown root:s3-orchestrator /etc/s3-orchestrator/tls/*
sudo chmod 0640 /etc/s3-orchestrator/tls/*
```

Certificates are reloaded on `SIGHUP`, so you can renew them (e.g., with certbot) and reload without downtime:

```bash
sudo systemctl reload s3-orchestrator
```

## Adding Encryption

For server-side encryption with a local master key:

```bash
# Generate a 256-bit master key
openssl rand 32 | base64 > /tmp/encryption.key

# Store it securely
sudo install -o root -g s3-orchestrator -m 0640 /tmp/encryption.key /etc/s3-orchestrator/encryption.key
rm /tmp/encryption.key
```

Add to `config.yaml`:

```yaml
encryption:
  enabled: true
  master_key_file: "/etc/s3-orchestrator/encryption.key"
```

For Vault Transit encryption, see the [Deploying on Nomad with Vault](../nomad-vault-deployment/) guide — the encryption config is the same regardless of deployment method.

## Upgrades

To upgrade, download the new `.deb` and install it over the existing package:

```bash
sudo dpkg -i s3-orchestrator_new-version_amd64.deb
sudo systemctl restart s3-orchestrator
```

The package uses `config|noreplace` for `/etc/s3-orchestrator/config.yaml` and `/etc/default/s3-orchestrator`, so your configuration is preserved across upgrades. Database migrations are applied automatically on startup.

## Important Notes

- The service does not start automatically after installation — configure it first, then run `systemctl start`
- Environment variables in `/etc/default/s3-orchestrator` are expanded in `config.yaml` at startup, not on reload — restart the service after changing environment variables
- The `s3-orchestrator` user has no login shell (`/usr/sbin/nologin`) and no home directory creation — it exists solely for service isolation
- Logs go to the systemd journal by default — use `journalctl -u s3-orchestrator` to view them
- Pin to a specific version when deploying to production rather than always using the latest release
