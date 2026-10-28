# Deployment

The Helm chart, the Nomad pack and the Nomad job that deploy s3-orchestrator, and local demos that run each platform end to end. All three read a `config.yaml` that references its secrets as `${NAME}`, and supply the secrets as environment variables. Each needs a PostgreSQL database and at least one S3-compatible backend, and Redis to run more than one instance.

## Kubernetes (Helm)

The chart in [`helm/s3-orchestrator`](helm/s3-orchestrator/README.md) is published to `oci://ghcr.io/afreidah/charts/s3-orchestrator` with each release and signed with cosign.

```bash
helm install s3-orchestrator oci://ghcr.io/afreidah/charts/s3-orchestrator \
  --version 0.178.0 -n s3-orchestrator --create-namespace -f my-values.yaml
```

`make helm-check` lints the chart and validates what it renders for each file in `helm/s3-orchestrator/ci`.

## Nomad

The pack in [`nomad/packs/s3_orchestrator`](nomad/packs/s3_orchestrator/README.md) is attached to each GitHub Release, stamped with that release:

```bash
nomad-pack run ./s3_orchestrator --var config_file=./config.yaml
```

[`nomad/s3-orchestrator.nomad.hcl`](nomad/s3-orchestrator.nomad.hcl) is the same job as a plain job file that takes variables. Production values, with Consul, Traefik and Vault, are in `nomad/s3-orchestrator.vars.hcl`:

```bash
nomad job run -var-file=s3-orchestrator.vars.hcl -var=version=v0.178.0 s3-orchestrator.nomad.hcl
```

`make pack-check` format-checks both and validates the jobs they produce.

## Local Demo Scripts

Both platforms include a `local/demo.sh` script that stands up a fully working environment on your machine with zero configuration. The two demos are the same environment on different schedulers: they share `deploy/local/config.yaml` and the helpers in `deploy/local/lib.sh`, so perf results from one are comparable with the other.

Each script runs PostgreSQL, Redis, three MinIO backends and the observability stack in the scheduler it targets, builds the image from source, and runs three orchestrator instances by default with three backends and replication factor 2. The instances share PostgreSQL and Redis and sit behind Traefik on port 9000; the Traefik dashboard is on port 8081. Set `INSTANCES` to change the count. Each script mints a root keypair and a `perf` identity per run, and prints connection details on success.

The Kubernetes demo installs the Helm chart and the Nomad demo runs the pack, each with the values a demo needs, so a demo run exercises what operators deploy.

`docker-compose.test.yml` is only for `make run` and `make dev-deps` now. It publishes the same host ports the demos use, so stop it with `make dev-clean` before starting a demo; the scripts refuse to start while it is up.

### Kubernetes (k3d)

```bash
./deploy/kubernetes/local/demo.sh               # stand up everything
INSTANCES=1 ./deploy/kubernetes/local/demo.sh   # one orchestrator pod
./deploy/kubernetes/local/demo.sh down          # tear it all down
```

Requires: `docker`, `k3d`, `kubectl`, `helm`, `jq`

Installs the Helm chart with the shared config passed as `configFile`. PostgreSQL, Redis and MinIO (`services.yaml`) and Prometheus, Grafana, Tempo and Loki (`observability.yaml`) run in a `demo` namespace, and the pods reach them by cluster DNS name. Traefik routes from Kubernetes Ingress, published through k3d's load balancer along with Grafana, Prometheus and Tempo. Alloy scrapes each pod's metrics listener in the cluster and remote-writes to Prometheus. A cluster created by an older version of the demo lacks the extra port mappings; run `down` once to recreate it.

### Nomad (dev mode)

```bash
./deploy/nomad/local/demo.sh               # stand up everything
INSTANCES=1 ./deploy/nomad/local/demo.sh   # one orchestrator instance
./deploy/nomad/local/demo.sh down          # tear it all down
```

Requires: `docker`, `nomad`, `nomad-pack`, `jq`, `curl`

Runs the pack with `local/s3-orchestrator.vars.hcl` and the shared config passed as `config_file`. PostgreSQL, Redis and MinIO run as the `demo-services` job (`services.nomad.hcl`), and the instances find them through Nomad's native service registry; `minio-setup.nomad.hcl` creates the buckets. Prometheus, Grafana, Tempo, Loki and Alloy run as the `demo-observability` job with the configs in `deploy/monitoring`. Traefik routes from the same registry. Each instance publishes its metrics listener on a dynamic port; Prometheus discovers them from the registry, and `demo.sh` prints their addresses. The script starts its own dev agent with `agent.hcl`, which enables Docker volumes (the observability job's bind mounts and the named volumes PostgreSQL and MinIO keep their data on) and memory oversubscription, so the backing services can burst past their reservations. `demo.sh down` removes the volumes along with the jobs, and `make clean` runs it.

### Monitoring

The demo scripts automatically start Prometheus (port 19090), Grafana (port 13000) and Tempo (port 3200) alongside the backing services. Grafana is pre-configured with an anonymous admin session, the Prometheus datasource, and the s3-orchestrator dashboard from `grafana/s3-orchestrator.json`. No login required -- just open `http://localhost:13000` after the demo is running.

## Cloudflare Edge Proxy

`deploy/cloudflare-worker/` holds a Cloudflare Worker that fronts a Bandwidth Alliance backend so object egress leaves the provider to Cloudflare, which alliance members waive the transfer fee on. It verifies the caller against a proxy credential, rewrites `Host` to the native endpoint, and re-signs the request with the account credential -- without which a proxied CNAME breaks both bucket routing and the SigV4 signature.

```bash
make worker-check   # typecheck and test
make worker-deploy  # wrangler deploy
```

See `deploy/cloudflare-worker/README.md` for the credential model and the matching backend entry.

## Secrets and Files

Sensitive values in the config use `${VAR}` environment variable expansion. The orchestrator resolves them at startup, so secrets never appear in config files.

| | Secrets as environment | Files the config points at (TLS, CA bundles) |
|---|---|---|
| Helm chart | `secrets`, `existingSecret`, `extraEnv`, `extraEnvFrom` | `extraVolumes` and `extraVolumeMounts` |
| Nomad pack and job | `env_template`, from a Nomad Variable by default, or Vault or Consul KV | `templates`, rendered into `/secrets` |

## PostgreSQL

These examples assume an existing PostgreSQL instance. The orchestrator creates its schema automatically on startup -- no manual migration required. Point `database.host` at your PostgreSQL endpoint and ensure the configured user has `CREATE TABLE` privileges on the target database.
