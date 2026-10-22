---
description: "Outbound webhooks for object mutations and operational events, delivered from a durable outbox so nothing is lost across a restart."
title: "Webhook Notifications"
linkTitle: "Notifications"
---

Optional outbound webhooks for object mutations and operational events. Each event is written to a durable `notification_outbox` table once the change that raised it has happened, then a background drainer POSTs it as CloudEvents-formatted JSON to each configured endpoint. A queued event survives a restart and is retried until it is delivered or its endpoint's retries run out.

## Event categories

- **Data events** - S3-style object mutations (`s3:ObjectCreated:Put`, `s3:ObjectRemoved:Delete`, etc.) carrying the bucket and key.
- **Operational events** - backend health (`backend.circuit.opened`, `backend.capacity.warning`), integrity (`integrity.corruption_detected`), cleanup (`cleanup.exhausted`), replication and lifecycle completions.

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
| `secret` | (Optional) HMAC-SHA256 signing key. The drainer adds `X-Webhook-Signature: sha256=<hex>` so the receiver can verify. |
| `timeout` | Per-POST request timeout (default 5s). |
| `max_retries` | Delivery attempts before the row is dropped (default 3). Failed deliveries retry with exponential backoff from one second, capped at 64 seconds. Any value is honored; there is no lower outbox-wide limit. |

## Delivery semantics

- An event is appended to `notification_outbox` once the change that raised it has happened, not inside that change's transaction.
- The notification drainer ticks every 2 seconds under advisory lock 1014, so one instance drains the outbox at a time, up to 50 rows per tick.
- Failed deliveries retry with exponential backoff. After `max_retries`, the row is dropped, an error is logged, and the drop is counted in `s3o_notification_dropped_total`.
- A row is deleted after a successful POST. If that delete fails, the row is delivered again on a later tick, so receivers should treat the event `id` as the deduplication key.

## Operational observability

| Metric | Meaning |
|---|---|
| `s3o_notification_queue_depth` | Notifications still to be delivered, including those waiting out a retry backoff |
| `s3o_notification_sent_total` | Successful POSTs by endpoint and event type |
| `s3o_notification_failed_total` | POST failures (counted before retry) |
| `s3o_notification_dropped_total` | Events dropped: rows that exceeded `max_retries`, capacity warnings suppressed as repeats, and events that could not be queued |
| `s3o_notification_store_errors_total` | Outbox reads or writes that failed, by operation. Non-zero means events are being lost before anyone tries to deliver them |
| `s3o_notification_duration_seconds` | POST latency by endpoint. A slow endpoint backs the queue up, so watch it alongside the depth gauge |

See [docs/monitoring.md](monitoring.md) for the full metric set.
