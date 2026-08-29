---
title: "Webhook Notifications"
linkTitle: "Notifications"
weight: 33
---

Optional outbound webhooks for object mutations and operational events. Events are written to a durable `notification_outbox` table inside the same transaction as the originating change, then a background drainer POSTs them as CloudEvents-formatted JSON to each configured endpoint. The outbox pattern means events are never lost on crash and never sent twice for the same change.

## Event categories

- **Data events** — S3-style object mutations (`s3:ObjectCreated:Put`, `s3:ObjectRemoved:Delete`, etc.) carrying the bucket and key.
- **Operational events** — backend health (`backend.circuit.opened`, `backend.capacity.warning`), integrity (`integrity.corruption_detected`), cleanup (`cleanup.exhausted`), replication and lifecycle completions.

The full event catalog and signature-verification recipe live in the [Event Notifications guide](/guides/event-notifications/) on the project website.

## Configuration

Each endpoint declares which event-type patterns it cares about and an optional HMAC-SHA256 signing key:

```yaml
notifications:
  endpoints:
    - url: "https://hooks.example.com/storage"
      events:
        - "s3:ObjectCreated:*"
        - "s3:ObjectRemoved:*"
      prefix: "uploads/"          # only deliver data events under this prefix
      secret: "${HOOK_SECRET}"
      timeout: 5s
      max_retries: 5
```

### Fields

| Field | Description |
|---|---|
| `url` | Endpoint that receives `POST` with CloudEvents JSON. |
| `events` | List of event-type glob patterns. `s3:ObjectCreated:*` matches Put / Copy / CompleteMultipartUpload. |
| `prefix` | (Optional) Only deliver data events whose key starts with this prefix. |
| `secret` | (Optional) HMAC-SHA256 signing key. The drainer adds `X-Signature-256: sha256=<hex>` so the receiver can verify. |
| `timeout` | Per-POST request timeout. |
| `max_retries` | Failed deliveries retry with exponential backoff up to this count, then the row is dropped and an audit warning is emitted. |

## Delivery semantics

- Events are appended to `notification_outbox` in the same DB transaction as the originating mutation. If the transaction rolls back, no event is queued.
- The notification drainer ticks every 5 seconds (no advisory lock — one drainer per instance is fine because each row is claimed via row-level locking before POST).
- Failed deliveries retry with exponential backoff. After `max_retries`, the row is dropped and an audit warning emitted; counted in the `s3o_notifications_dropped_total` Prometheus metric.
- The drainer never sends the same event twice — `notification_outbox` rows are deleted on successful POST and the row claim survives crashes.

## Operational observability

| Metric | Meaning |
|---|---|
| `s3o_notification_outbox_depth` | Current queued rows |
| `s3o_notifications_delivered_total` | Successful POSTs by endpoint |
| `s3o_notifications_failed_total` | POST failures (counted before retry) |
| `s3o_notifications_dropped_total` | Rows that exceeded `max_retries` and were dropped |

See [docs/monitoring.md](monitoring.md) for the full metric set.
