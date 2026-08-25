---
title: "Admin Guide"
linkTitle: "Admin Guide"
---

The admin guide has been split into per-topic pages so each topic has a focused home rather than living in a single 2000-line reference. Start with the topic that matches what you're doing:

## Getting started

- **[Quickstart](quickstart.md)** — get a working deployment in under a minute
- **[Architecture](architecture.md)** — system diagram, storage layer, write routing

## Configuration

- **[Configuration walkthrough](configuration.md)** — every YAML block, validation rules, hot-reload matrix
- **[Authentication](authentication.md)** — SigV4 variants, credentials, multi-bucket isolation
- **[Backends](backends.md)** — provider quick-reference, routing strategies, usage limits, multi-backend topologies
- **[Database](database.md)** — engine choice (SQLite vs PostgreSQL), schema, goose migrations
- **[Encryption](encryption.md)** — envelope encryption, Vault Transit, key rotation, integrity verification
- **[Notifications](notifications.md)** — webhook outbox pattern, CloudEvents JSON, HMAC signing

## Operations

- **[Operations](operations.md)** — drain, rebalance, scrub, cache management, trace snapshot, credential rotation
- **[Replication](replication.md)** — replication factor, over-replication cleanup, orphan reconciliation
- **[Cleanup and lifecycle](cleanup-and-lifecycle.md)** — cleanup queue, PUT-before-COMMIT pending intents, lifecycle expiration
- **[CLI subcommands](cli.md)** — `version`, `init`, `validate`, `sync`, `admin`, `tui`
- **[Deployment](deployment.md)** — Nomad, Kubernetes, Docker, Debian package, multi-instance

## Observability and troubleshooting

- **[Monitoring](monitoring.md)** — full Prometheus metric reference, OpenTelemetry traces, structured logs, audit log, panic recovery, on-demand reconciliation
- **[Background services](background-services.md)** — worker reference table (intervals, advisory locks)
- **[Performance tuning](performance-tuning.md)** — connection pools, timeouts, broadcast tuning
- **[Disaster recovery](disaster-recovery.md)** — failure scenarios and recovery procedures
- **[Security hardening](security-hardening.md)** — TLS, mTLS, config security, network segmentation
- **[API reference](api-reference.md)** — UI and Admin API JSON endpoints
