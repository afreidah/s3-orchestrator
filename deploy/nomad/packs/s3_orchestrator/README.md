# s3-orchestrator Nomad Pack

Runs [s3-orchestrator](https://s3-orchestrator.munchbox.cc) as a Nomad service job: one S3-compatible endpoint in front of several storage backends, with quotas, replication and usage limits. The pack runs your `config.yaml` with its secrets supplied as environment variables, registers the S3 listener and a metrics listener as services, rolls out one instance at a time behind its readiness check, and drains in-flight requests before the task stops. The container runs on a read-only root filesystem with every capability dropped.

It needs Nomad 1.10 or later, the Docker driver, clients whose `max_kill_timeout` is at least 90 seconds (see [Shutdown](#shutdown)), and a PostgreSQL database the clients can reach. The pack refuses a config that selects SQLite.

## Install

The pack runs the `config.yaml` you point it at. Keep secrets out of the file and reference them as environment variables, which the orchestrator expands at startup. See the [configuration reference](https://github.com/afreidah/s3-orchestrator/blob/main/docs/configuration.md) for every field.

```yaml
# config.yaml
server:
  listen_addr: "0.0.0.0:9000"
database:
  host: "postgres.example.internal"
  user: "s3_orchestrator"
  password: "${DB_PASSWORD}"
# ...
telemetry:
  metrics:
    enabled: true
    listen: "0.0.0.0:9001"
```

By default the task's environment is every item of the Nomad Variable at `nomad/jobs/<job name>`, which the job can read with no extra policy. Create it, then run the pack:

```bash
nomad var put nomad/jobs/s3-orchestrator DB_PASSWORD=... BACKEND_SECRET_KEY=... UI_SESSION_SECRET=...

nomad-pack run s3_orchestrator --var version=v0.191.0 --var config_file=./config.yaml
```

The file is read when the pack renders, on the machine running `nomad-pack`, and placed in the job as written. Nomad renders it as a template, so it can also read Vault, Consul KV or service addresses itself with `{{ }}`. The task waits to start until the Nomad Variable exists.

A pack taken from a release, or from a tag of [afreidah/nomad-pack-s3-orchestrator](https://github.com/afreidah/nomad-pack-s3-orchestrator), defaults `version` to that release. Run from a source checkout of s3-orchestrator, it has no default and refuses to render without one.

Keep `server.listen_addr` on port 9000, and `telemetry.metrics.listen` on `0.0.0.0:9001` while `register_metrics_service` is on. Those are the container ports the pack maps.

## Secrets From Vault

Point `env_template` at Vault and name the role the task logs in with through workload identity:

```hcl
# vars.hcl
vault_role = "s3-orchestrator"

env_template = <<EOH
{{ with secret "secret/data/s3-orchestrator" }}
DB_PASSWORD={{ .Data.data.db_password }}
BACKEND_SECRET_KEY={{ .Data.data.backend_secret_key }}
UI_SESSION_SECRET={{ .Data.data.ui_session_secret }}
{{ end }}
EOH
```

```bash
nomad-pack run s3_orchestrator --var version=v0.191.0 --var config_file=./config.yaml --var-file vars.hcl
```

Workload identity needs `vault.default_identity` in the Nomad servers' Vault configuration. `env_template` is an ordinary Nomad template, so it can also read Consul KV with `{{ key }}`, or mix sources. Setting `vault_role` also gives the task a Vault token at `/secrets/vault_token`, which `encryption.vault.token_file` can point at for Vault Transit.

## Files the Config Points At

Settings that take a path, such as `server.tls.client_ca_file`, `encryption.vault.ca_cert`, `encryption.master_key_file` and `encryption.previous_key_files`, read files rendered by `templates`. Each entry is a Nomad template written to `/secrets/<destination>`, and a change restarts the task. A key file holds the raw 32 bytes, so decode a stored base64 key in the template:

```hcl
# vars.hcl
templates = [
  {
    destination = "master.key"
    data        = "{{ with nomadVar \"nomad/jobs/s3-orchestrator/encryption\" }}{{ .key | base64Decode }}{{ end }}"
  },
]
```

```yaml
# config.yaml
encryption:
  enabled: true
  master_key_file: "/secrets/master.key"
```

## TLS

The certificate and key go in `cert_templates` instead. A renewal sends SIGHUP, which reloads them without restarting the task. Set `tls_mode` to match the config: `tls` makes the health checks connect over HTTPS, and `mtls`, for a config with `client_ca_file`, makes them TCP checks, since they cannot present a client certificate.

```hcl
# vars.hcl
tls_mode = "tls"

cert_templates = [
  {
    destination = "tls.crt"
    data        = "{{ with nomadVar \"nomad/jobs/s3-orchestrator/tls\" }}{{ .cert }}{{ end }}"
  },
  {
    destination = "tls.key"
    data        = "{{ with nomadVar \"nomad/jobs/s3-orchestrator/tls\" }}{{ .key }}{{ end }}"
  },
]
```

```yaml
# config.yaml
server:
  tls:
    cert_file: "/secrets/tls.crt"
    key_file: "/secrets/tls.key"
```

## Running More Than One Instance

Raise `count` only with `redis` in the config. Without it every instance enforces usage limits against its own counters, and the fleet overshoots each limit by about the instance count. The pack refuses to render that combination.

A rollout starts with one canary, which needs room for an extra instance. With `distinct_hosts` or a static `http_port` there is no spare placement, so the rollout replaces instances one at a time instead.

To split request serving from the background work, run the pack twice under different job names, one with `mode = "api"` and one with `mode = "worker"`. Give the worker job no router `service_tags`: it serves no S3 routes, so traffic routed to it is answered with 404.

## Spill Directory

Uploads over 32 MiB are buffered to a file in `/tmp` while they are written. The root filesystem is read-only, so the pack binds `/tmp` to the task's tmp directory on the client's disk. `ephemeral_disk` reserves room for it at placement; size it for `server.max_concurrent_writes` times the largest upload you expect. Nomad does not enforce the size.

## Shutdown

Nomad deregisters the services, waits `shutdown_delay`, then sends SIGTERM. The orchestrator then drains HTTP requests for up to 30 seconds, uploads still writing for up to 30 more, and background services for up to 10. `kill_timeout` defaults to 90 seconds to cover all of it. A client caps `kill_timeout` at its `max_kill_timeout`, which defaults to 30 seconds, so raise it in the client configuration:

```hcl
client {
  max_kill_timeout = "90s"
}
```

## Variables

| Variable | Default | Description |
|---|---|---|
| `config_file` | required | Path to the `config.yaml` to run |
| `config_wait` | off | Quiet period `{ min, max }` before a changed config re-renders |
| `env` | `{}` | Environment variables set on the task as given |
| `env_template` | the job's Nomad Variable | Nomad template rendered as the task's environment |
| `job_name` | `s3-orchestrator` | Job ID, and the Nomad Variable path the default environment reads |
| `region` | agent's | Region to run in |
| `namespace` | `default` | Namespace to run in |
| `datacenters` | `["*"]` | Datacenters eligible for placement |
| `node_pool` | `default` | Node pool eligible for placement |
| `count` | `1` | Number of instances; more than one requires `redis` |
| `distinct_hosts` | `false` | Place each instance on a different client node |
| `constraints` | `[]` | Extra job constraints, `[{ attribute, operator, value }]` |
| `image` | `ghcr.io/afreidah/s3-orchestrator` | Container image repository |
| `version` | the release | Container image tag |
| `mode` | `all` | `all`, `api` or `worker` |
| `http_port` | `0` | Static host port for the S3 listener; `0` is dynamic |
| `tls_mode` | `off` | `off`, `tls` or `mtls`, matching `server.tls` in the config |
| `service_provider` | `nomad` | `nomad` or `consul` |
| `service_name` | job name | Service name for the S3 listener |
| `service_tags` | `[]` | Tags on the S3 service, such as Traefik router rules |
| `register_metrics_service` | `true` | Register `<service name>-metrics` on port 9001 |
| `resources` | `1000` MHz, `1024` MiB | `cpu`, `memory` and `memory_max` (`0` leaves oversubscription off) |
| `ephemeral_disk` | `300` | Disk in MB reserved for spill files |
| `shutdown_delay` | `5s` | Time between deregistering and SIGTERM |
| `kill_timeout` | `90s` | Time between SIGTERM and SIGKILL, capped by the client's `max_kill_timeout` |
| `templates` | `[]` | Extra files rendered into `/secrets`, `[{ data, destination }]`; a change restarts the task |
| `cert_templates` | `[]` | TLS certificate and key rendered into `/secrets`, `[{ data, destination }]`; a renewal sends SIGHUP |
| `vault_role` | none | Vault role for workload identity |

The process sets its Go memory limit to 90% of the memory limit it runs under, so `GOMEMLIMIT` needs no setting. Nomad's `cpu` is a scheduling share rather than a limit, so Go sizes `GOMAXPROCS` to every core on the node; set it through `env` to match the reservation.
