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

## Kubernetes

Separate manifests for each resource type. Apply in order:

```bash
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -f deploy/kubernetes/secret.yaml
kubectl apply -f deploy/kubernetes/serviceaccount.yaml
kubectl apply -f deploy/kubernetes/configmap.yaml
kubectl apply -f deploy/kubernetes/deployment.yaml
kubectl apply -f deploy/kubernetes/service.yaml

# Optional: external access via Ingress
kubectl apply -f deploy/kubernetes/ingress.yaml
```

**Prerequisites:** A PostgreSQL instance accessible from the cluster and a populated Secret with all credential fields. See `secret.yaml` for the full list.

## Local Demo Scripts

Both platforms include a `local/demo.sh` script that stands up a fully working environment on your machine with zero configuration. Each script starts PostgreSQL and MinIO via docker-compose, builds the image from source, and deploys the orchestrator with two backends and replication factor 2.

### Kubernetes (k3d)

```bash
./deploy/kubernetes/local/demo.sh        # stand up everything
./deploy/kubernetes/local/demo.sh down   # tear it all down
```

Requires: `docker`, `k3d`, `kubectl`

### Nomad (dev mode)

```bash
./deploy/nomad/local/demo.sh        # stand up everything
./deploy/nomad/local/demo.sh down   # tear it all down
```

Requires: `docker`, `nomad`

Both scripts print connection details on success -- S3 API endpoint, dashboard URL, and a test upload command.

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
