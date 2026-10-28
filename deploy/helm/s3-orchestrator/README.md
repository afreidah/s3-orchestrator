# s3-orchestrator Helm Chart

Runs [s3-orchestrator](https://s3-orchestrator.munchbox.cc) on Kubernetes: one S3-compatible endpoint in front of several storage backends, with quotas, replication and usage limits. The chart deploys the orchestrator itself. It needs a PostgreSQL database and at least one S3-compatible backend that already exist, and Redis to run more than one replica.

Requires Kubernetes 1.29 or later and Helm 3.8 or later.

## Install

The chart is published to GitHub Container Registry with each release, at the same version as the image it runs:

```bash
helm install s3-orchestrator oci://ghcr.io/afreidah/charts/s3-orchestrator \
  --version 0.178.0 -n s3-orchestrator --create-namespace -f my-values.yaml
```

Releases are signed with cosign through GitHub Actions. To verify one before installing:

```bash
cosign verify ghcr.io/afreidah/charts/s3-orchestrator:0.178.0 \
  --certificate-identity-regexp '^https://github.com/afreidah/s3-orchestrator/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Configuration and Secrets

The orchestrator's `config.yaml` comes from `config`, or from `configFile` to use a complete file you already have:

```bash
helm install s3-orchestrator oci://ghcr.io/afreidah/charts/s3-orchestrator \
  --version 0.178.0 --set-file configFile=config.yaml -f my-values.yaml
```

Keep secrets out of the config. Reference them as `${NAME}`, and supply them as environment variables from the Secret the chart renders from `secrets`, or from your own Secret named in `existingSecret`:

```yaml
# my-values.yaml
existingSecret: s3-orchestrator-credentials

config:
  database:
    host: postgres.databases.svc
    password: "${DB_PASSWORD}"
  backends:
    - name: primary
      endpoint: https://s3.example.com
      region: us-east-1
      bucket: my-bucket
      access_key_id: "${BACKEND1_ACCESS_KEY}"
      secret_access_key: "${BACKEND1_SECRET_KEY}"
      quota_bytes: 10737418240
```

Settings that take a file path, such as `server.tls.cert_file`, read files mounted through `extraVolumes` and `extraVolumeMounts`. `values.yaml` shows a TLS Secret mounted this way.

## Exposing the S3 API

Use `httpRoute` to route through an existing Gateway API Gateway, or `ingress` for an Ingress controller. Large uploads need the route or controller to allow long requests and large bodies: `httpRoute.timeouts.request`, or the controller's own annotations under `ingress.annotations`.

## Running More Than One Replica

Set `replicaCount` above 1, or turn on `autoscaling`, only with `redis` in the config. Without it each replica enforces usage limits against its own counters, and the chart refuses to render. A PodDisruptionBudget keeps a node drain from evicting more than one replica at a time.

To split request serving from background work, install the chart twice, once with `mode: api` and once with `mode: worker`.

## Uploads and Disk

Uploads over 32 MiB buffer in a spill directory at `/tmp`, an `emptyDir` sized by `spill.sizeLimit`. It holds one file per concurrent large upload, each up to the object's size, so size it for `max_concurrent_writes` times the largest object you expect, and keep `resources.limits.ephemeral-storage` above it. `spill.medium: Memory` keeps it in RAM instead, where it counts against `resources.limits.memory`.

## Values

`values.yaml` documents every value, and `values.schema.json` rejects unknown keys and invalid values at install. `helm show values oci://ghcr.io/afreidah/charts/s3-orchestrator --version 0.178.0` prints the defaults for a release.

The process sets its Go memory limit to 90% of `resources.limits.memory`, so no `GOMEMLIMIT` setting is needed.
