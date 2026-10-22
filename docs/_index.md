---
title: "Documentation"
linkTitle: "Docs"
description: "Configuration, deployment, backends, replication, encryption, compression, monitoring, and the admin API for s3-orchestrator."
weight: 20
# Sidebar order, in the groups of the table below, and titles for the pages
# whose front matter has none. A page that sets its own title keeps it.
cascade:
  - {weight: 101, title: "Quickstart", target: {path: "/docs/quickstart"}}
  - {weight: 102, title: "README", target: {path: "/docs/readme"}}
  - {weight: 103, title: "User Guide", target: {path: "/docs/user-guide"}}
  - {weight: 104, target: {path: "/docs/architecture"}}
  - {weight: 201, target: {path: "/docs/configuration"}}
  - {weight: 202, target: {path: "/docs/backends"}}
  - {weight: 203, target: {path: "/docs/authentication"}}
  - {weight: 204, target: {path: "/docs/database"}}
  - {weight: 205, target: {path: "/docs/encryption"}}
  - {weight: 206, target: {path: "/docs/compression"}}
  - {weight: 207, target: {path: "/docs/tagging"}}
  - {weight: 208, target: {path: "/docs/notifications"}}
  - {weight: 301, target: {path: "/docs/admin-guide"}}
  - {weight: 302, target: {path: "/docs/operations"}}
  - {weight: 303, target: {path: "/docs/replication"}}
  - {weight: 304, target: {path: "/docs/cleanup-and-lifecycle"}}
  - {weight: 305, target: {path: "/docs/background-services"}}
  - {weight: 306, target: {path: "/docs/deployment"}}
  - {weight: 307, target: {path: "/docs/cli"}}
  - {weight: 308, target: {path: "/docs/monitoring"}}
  - {weight: 309, target: {path: "/docs/grafana-dashboard"}}
  - {weight: 310, title: "Disaster Recovery", target: {path: "/docs/disaster-recovery"}}
  - {weight: 311, title: "Security Hardening", target: {path: "/docs/security-hardening"}}
  - {weight: 401, title: "Performance Tuning", target: {path: "/docs/performance-tuning"}}
  - {weight: 402, target: {path: "/docs/performance-envelope"}}
  - {weight: 403, target: {path: "/docs/benchmarking"}}
  - {weight: 501, target: {path: "/docs/admin-api"}}
  - {weight: 502, title: "API Reference", target: {path: "/docs/api-reference"}}
  - {weight: 503, title: "Version Migration", target: {path: "/docs/version-migration"}}
  - {weight: 504, title: "Changelog", target: {path: "/docs/changelog"}}
  - {weight: 601, title: "Style Guide", target: {path: "/docs/style-guide"}}
  - {weight: 602, target: {path: "/docs/contributing/logging"}}
  - {weight: 603, target: {path: "/docs/contributing/store"}}
---

s3-orchestrator is a storage orchestration layer that presents any number of
S3-compatible providers as one S3 endpoint. It tracks where every object lives
in its own database, and from that places each write, keeps the configured
number of copies, enforces per-backend quotas and usage limits, and keeps
serving when a backend does not.

| Section | Page | Covers |
|---|---|---|
| Getting started | [Quickstart](quickstart.md) | A local instance and a first object, in under a minute |
| | [README](readme.md) | Project overview, features and install |
| | [User Guide](user-guide.md) | Connecting the AWS CLI, rclone, SDKs and other S3 clients |
| | [Architecture](architecture.md) | Components, the storage layer and write routing |
| Configuring | [Configuration](configuration.md) | Every block of the configuration file |
| | [Backends](backends.md) | Providers, routing strategies, quotas and usage limits |
| | [Authentication](authentication.md) | SigV4, credentials and bucket isolation |
| | [Database](database.md) | SQLite or PostgreSQL, schema and migrations |
| | [Encryption](encryption.md) | Envelope encryption, Vault Transit and key rotation |
| | [Compression](compression.md) | Chunked zstd and ranged reads |
| | [Tagging](tagging.md) | Object tag sets and how they are stored |
| | [Notifications](notifications.md) | Webhook outbox, CloudEvents and HMAC signing |
| Operating | [Admin Guide](admin-guide.md) | Index of the operational pages |
| | [Operations](operations.md) | Cordon, drain, rebalance, scrub and cache management |
| | [Replication](replication.md) | Replication factor, over-replication and reconciliation |
| | [Cleanup and lifecycle](cleanup-and-lifecycle.md) | Cleanup queue, pending intents and expiration |
| | [Background services](background-services.md) | Every periodic worker, its interval and lock |
| | [Deployment](deployment.md) | Nomad, Kubernetes, Docker, the Debian package and multi-instance |
| | [CLI and TUI](cli.md) | Every subcommand and the terminal UI |
| | [Monitoring](monitoring.md) | Metrics, traces, structured logs and the audit log |
| | [Grafana Dashboard](grafana-dashboard.md) | The bundled dashboard, shown whole |
| | [Disaster Recovery](disaster-recovery.md) | Failure scenarios and recovery procedures |
| | [Security Hardening](security-hardening.md) | TLS, credentials, network isolation and encryption |
| Performance | [Performance Tuning](performance-tuning.md) | Connection pools, timeouts, caching and sizing |
| | [Performance envelope](performance-envelope.md) | Measured throughput and latency |
| | [Benchmarking](benchmarking.md) | Micro-benchmarks for hot-path regressions |
| Reference | [Admin API](admin-api.md) | The control plane, generated from the route table |
| | [API Reference](api-reference.md) | The dashboard's JSON API |
| | [Version Migration](version-migration.md) | Upgrade notes, breaking changes and migrations |
| | [Changelog](changelog.md) | Release history |
| Contributing | [Style Guide](style-guide.md) | Code, comment and documentation conventions |
| | [Logging](contributing/logging.md) | Structured log conventions |
| | [Metadata store](contributing/store.md) | Store interface conventions |
