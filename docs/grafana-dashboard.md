---
title: "Grafana Dashboard"
description: "The full Grafana dashboard that ships with s3-orchestrator: requests, latency, backend health, quota and usage, replication and background tasks."
---

The Grafana dashboard that ships at `grafana/s3-orchestrator.json`, shown whole. Import it into Grafana with a Prometheus datasource scraping the orchestrator; see [Monitoring](monitoring.md) for the metrics behind each panel.

![The whole bundled Grafana dashboard, from build info and request rates through quota, backend health, replication, background tasks, encryption and compression](/docs/images/grafana-full.png)
