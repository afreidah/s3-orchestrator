# Deployment Examples

Production-ready deployment manifests for container orchestration platforms. Both examples deploy the s3-orchestrator with three storage backends (OCI Object Storage, Backblaze B2, MinIO), replication factor 2, spread routing, and full observability -- demonstrating how the orchestrator distributes object copies across backends like a storage mesh.

## Nomad

Single-file job specification with Vault integration for secret injection.

```bash
# Deploy with a specific version
nomad job run -var="version=v1.0.0" deploy/nomad/s3-orchestrator.nomad.hcl

# Validate without deploying
nomad job plan -var="version=v1.0.0" deploy/nomad/s3-orchestrator.nomad.hcl
```

**Prerequisites:** Vault KV v2 secret at `secret/data/s3-orchestrator` with all credential fields populated. See the inline comments in the job file for the full list of required keys.

## Kubernetes (Helm)

A Helm chart in `deploy/helm/s3-orchestrator/` deploys the full stack: Deployment, Service, ConfigMap, Secret, ServiceAccount, NetworkPolicy, and optional Ingress.

```bash
# Install with default values
helm install s3-orchestrator deploy/helm/s3-orchestrator \
  -n s3-orchestrator --create-namespace

# Install with custom values
helm install s3-orchestrator deploy/helm/s3-orchestrator \
  -n s3-orchestrator --create-namespace \
  -f my-values.yaml

# Upgrade after changing values
helm upgrade s3-orchestrator deploy/helm/s3-orchestrator \
  -n s3-orchestrator -f my-values.yaml

# Generate raw manifests without Helm (for kubectl apply workflows)
helm template s3-orchestrator deploy/helm/s3-orchestrator -f my-values.yaml > manifests.yaml
```

**Prerequisites:** A PostgreSQL instance accessible from the cluster. Configure backends, credentials, and database connection in your values file. See `deploy/helm/s3-orchestrator/values.yaml` for all available options.

## Local Demo Scripts

Both platforms include a `local/demo.sh` script that stands up a fully working environment on your machine with zero configuration. The two demos are the same environment on different schedulers: they share `deploy/local/config.yaml` and the helpers in `deploy/local/lib.sh`, so perf results from one are comparable with the other.

Each script starts PostgreSQL, Redis and MinIO via docker-compose, builds the image from source, and runs three orchestrator instances by default with three backends and replication factor 2. The instances share PostgreSQL and Redis and sit behind Traefik on port 9000; the Traefik dashboard is on port 8081. Set `INSTANCES` to change the count. Each script mints a root keypair and a `perf` identity per run, and prints connection details on success.

These are demos, not deployment templates. The production examples are `deploy/nomad/s3-orchestrator.nomad.hcl` and the Helm chart in `deploy/helm/s3-orchestrator`.

### Kubernetes (k3d)

```bash
./deploy/kubernetes/local/demo.sh               # stand up everything
INSTANCES=1 ./deploy/kubernetes/local/demo.sh   # one orchestrator pod
./deploy/kubernetes/local/demo.sh down          # tear it all down
```

Requires: `docker`, `k3d`, `kubectl`, `helm`, `jq`

Installs the Helm chart with the shared config passed as `configFile`. Traefik routes from Kubernetes Ingress, published through k3d's load balancer. Alloy scrapes each pod's metrics listener in the cluster and remote-writes to Prometheus.

### Nomad (dev mode)

```bash
./deploy/nomad/local/demo.sh               # stand up everything
INSTANCES=1 ./deploy/nomad/local/demo.sh   # one orchestrator instance
./deploy/nomad/local/demo.sh down          # tear it all down
```

Requires: `docker`, `nomad`, `jq`

Traefik routes from Nomad's native service registry. Each instance publishes its metrics listener on a dynamic port; Prometheus discovers them from the same registry, and `demo.sh` prints their addresses.

### Monitoring

The demo scripts automatically start Prometheus (port 19090) and Grafana (port 13000) alongside the backing services. Grafana is pre-configured with an anonymous admin session, the Prometheus datasource, and the s3-orchestrator dashboard from `grafana/s3-orchestrator.json`. No login required -- just open `http://localhost:13000` after the demo is running.

## Cloudflare Edge Proxy

`deploy/cloudflare-worker/` holds a Cloudflare Worker that fronts a Bandwidth Alliance backend so object egress leaves the provider to Cloudflare, which alliance members waive the transfer fee on. It verifies the caller against a proxy credential, rewrites `Host` to the native endpoint, and re-signs the request with the account credential -- without which a proxied CNAME breaks both bucket routing and the SigV4 signature.

```bash
make worker-check   # typecheck and test
make worker-deploy  # wrangler deploy
```

See `deploy/cloudflare-worker/README.md` for the credential model and the matching backend entry.

## Customization

Both examples include commented-out sections for:

- **TLS termination** at the application level (cert + key file paths)
- **Mutual TLS (mTLS)** client certificate verification via `client_ca_file`
- **Traefik/nginx Ingress** routing annotations
- **Vault Agent Injector** integration (Kubernetes)
- **Distributed tracing** via OTLP gRPC (Tempo, Jaeger)

All sensitive values use `${VAR}` environment variable expansion -- the orchestrator resolves these at startup, so secrets never appear in config files on disk.

## PostgreSQL

These examples assume an existing PostgreSQL instance. The orchestrator creates its schema automatically on startup -- no manual migration required. Point `database.host` at your PostgreSQL endpoint and ensure the configured user has `CREATE TABLE` privileges on the target database.
