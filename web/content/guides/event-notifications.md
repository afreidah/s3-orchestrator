---
title: "Event Notifications"
weight: 4
---


This guide explains how to configure webhook notifications so external systems are informed when objects are created, deleted, or when operational events occur (circuit breaker trips, capacity warnings, replication completions, etc.).

## Overview

The S3 Orchestrator delivers [CloudEvents 1.0](https://cloudevents.io/) JSON payloads to one or more webhook endpoints via HTTP POST. Events are durably persisted in a notification outbox table before delivery, so notifications survive restarts and are retried with exponential backoff on failure. A background worker drains the outbox every 2 seconds under an advisory lock, making it safe to run multiple orchestrator instances.

Notifications are entirely optional. When no endpoints are configured, the notification subsystem is disabled and emits no events.

## Configuration

Add a `notifications` block to your configuration file. Each endpoint receives events matching its configured patterns.

### Minimal Example

```yaml
notifications:
  endpoints:
    - url: "https://hooks.example.com/s3-events"
      events:
        - "s3:ObjectCreated:*"
        - "s3:ObjectRemoved:*"
```

This sends all object creation and deletion events to a single webhook.

### Full Example

```yaml
notifications:
  endpoints:
    # Slack-style webhook for operational alerts
    - url: "https://hooks.slack.com/services/T00/B00/xxxx"
      events:
        - "backend.*"
        - "integrity.*"
        - "cleanup.exhausted"
      secret: "hmac-signing-key-for-slack"
      timeout: 10s
      max_retries: 5

    # Application webhook for object mutations in a specific prefix
    - url: "https://api.example.com/webhooks/storage"
      events:
        - "s3:ObjectCreated:*"
        - "s3:ObjectRemoved:*"
      prefix: "uploads/"
      secret: "app-webhook-secret"

    # Catch-all audit endpoint
    - url: "https://audit.internal/ingest"
      events:
        - "*"
```

### Configuration Reference

| Field | Default | Description |
|-------|---------|-------------|
| `url` | (required) | HTTP(S) endpoint URL to POST events to |
| `events` | (required) | List of event type patterns to match (see [Event Types](#event-types)) |
| `prefix` | (none) | Only deliver data events whose subject starts with this prefix |
| `secret` | (none) | HMAC-SHA256 signing key for payload verification |
| `timeout` | `5s` | HTTP request timeout per delivery attempt |
| `max_retries` | `3` | Maximum delivery attempts before the event is dropped |

## Event Types

Events fall into two categories: **data events** triggered by S3 operations, and **operational events** triggered by system state changes.

### Data Events

These follow the S3 notification naming convention and include the object key as the event subject.

| Event Type | Trigger |
|-----------|---------|
| `s3:ObjectCreated:Put` | Object uploaded via PUT |
| `s3:ObjectCreated:Copy` | Object created via server-side copy |
| `s3:ObjectCreated:CompleteMultipartUpload` | Multipart upload completed |
| `s3:ObjectRemoved:Delete` | Single object deleted |
| `s3:ObjectRemoved:DeleteBatch` | Batch delete operation |
| `lifecycle.delete` | Object removed by lifecycle expiration |

### Operational Events

These notify about infrastructure state changes. The subject field typically contains the backend name.

| Event Type | Trigger |
|-----------|---------|
| `backend.circuit.opened` | Backend circuit breaker tripped open |
| `backend.circuit.closed` | Backend circuit breaker recovered |
| `backend.capacity.warning` | Backend approaching quota limit (dampened to 1/hour) |
| `backend.drain.completed` | Backend drain finished |
| `backend.drain.failed` | Backend drain encountered an error |
| `backend.removed` | Backend removed after drain |
| `integrity.corruption_detected` | Scrubber found a hash mismatch |
| `cleanup.exhausted` | Cleanup queue item hit max retry attempts |
| `replication.target_exhausted` | No backend has space for a replica |
| `replication.completed` | Replication cycle finished |
| `rebalance.completed` | Rebalance cycle finished |
| `lifecycle.completed` | Lifecycle expiration cycle finished |
| `config.reload_failed` | SIGHUP config reload failed |
| `service.started` | Orchestrator started |
| `service.stopping` | Orchestrator shutting down |

### Wildcard Patterns

The `events` filter supports trailing wildcards:

| Pattern | Matches |
|---------|---------|
| `*` | All events |
| `s3:ObjectCreated:*` | All object creation events (Put, Copy, CompleteMultipartUpload) |
| `s3:ObjectRemoved:*` | All object deletion events (Delete, DeleteBatch) |
| `backend.*` | All backend state events (circuit, capacity, drain, removed) |
| `integrity.*` | Integrity corruption events |

## Payload Format

Events are delivered as CloudEvents 1.0 JSON. Here is an example payload for an object upload:

```json
{
  "specversion": "1.0",
  "id": "evt_a1b2c3d4e5f6a7b8c9d0e1f2",
  "source": "/s3-orchestrator",
  "type": "s3:ObjectCreated:Put",
  "time": "2026-03-28T14:30:00Z",
  "subject": "photos/vacation/sunset.jpg",
  "datacontenttype": "application/json",
  "data": {
    "bucket": "photos",
    "key": "photos/vacation/sunset.jpg",
    "backend": "r2",
    "size": 2048576,
    "request_id": "a1b2c3d4e5f6a7b8"
  }
}
```

Operational events include context-specific fields in the `data` object:

```json
{
  "specversion": "1.0",
  "id": "evt_f1e2d3c4b5a6f7e8d9c0b1a2",
  "source": "/s3-orchestrator",
  "type": "backend.circuit.opened",
  "time": "2026-03-28T14:35:00Z",
  "subject": "oci",
  "datacontenttype": "application/json",
  "data": {
    "backend": "oci",
    "reason": "5 consecutive failures"
  }
}
```

## Webhook Signature Verification

When a `secret` is configured for an endpoint, each delivery includes an `X-Webhook-Signature` header containing an HMAC-SHA256 signature of the request body:

```
X-Webhook-Signature: sha256=a1b2c3...
```

To verify on the receiving end:

```python
import hmac, hashlib

def verify(body: bytes, secret: str, signature: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode(), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)
```

```go
func verify(body []byte, secret, signature string) bool {
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(expected), []byte(signature))
}
```

## Delivery Guarantees

- **At-least-once delivery**: Events are persisted to the outbox before delivery. If the webhook returns a non-2xx response or the request times out, the event is retried with exponential backoff (1s, 2s, 4s, ..., up to 64s).
- **Ordering**: Events are delivered roughly in order but strict ordering is not guaranteed, especially during retries.
- **Idempotency**: Each event has a unique `id` field. Receivers should use this for deduplication if exactly-once semantics are needed.
- **Dampening**: Capacity warning events (`backend.capacity.warning`) are dampened per backend to at most once per hour to prevent notification spam when a backend hovers near its limit.
- **Multi-instance safety**: The outbox is drained under a PostgreSQL advisory lock, so only one instance delivers at a time regardless of how many are running.

## Prefix Filtering

The `prefix` field on an endpoint filters **data events only** (S3 object events). Operational events are always delivered if they match the event pattern, regardless of prefix.

```yaml
notifications:
  endpoints:
    # Only notify about uploads to the "invoices/" prefix
    - url: "https://billing.internal/hooks"
      events:
        - "s3:ObjectCreated:*"
      prefix: "invoices/"
```

An object uploaded as `invoices/2026/march.pdf` matches; `photos/cat.jpg` does not.

## Monitoring

The orchestrator exposes Prometheus metrics for notification delivery:

| Metric | Type | Description |
|--------|------|-------------|
| `s3o_notification_sent_total` | counter | Successful deliveries (labels: endpoint, event_type) |
| `s3o_notification_failed_total` | counter | Failed delivery attempts (labels: endpoint, event_type) |
| `s3o_notification_dropped_total` | counter | Events dropped (dampened or enqueue failure) |
| `s3o_notification_queue_depth` | gauge | Current outbox queue depth |
| `s3o_notification_duration_seconds` | histogram | Delivery latency (labels: endpoint) |

## Troubleshooting

**Events not arriving**: Check `s3o_notification_queue_depth` - if it is growing, deliveries are failing. Look at `s3o_notification_failed_total` for the endpoint and check the orchestrator logs for delivery error messages.

**Duplicate events**: This is expected behavior (at-least-once delivery). Use the `id` field for deduplication on the receiver side.

**Capacity warnings flooding**: These are automatically dampened to once per hour per backend. If you are still seeing too many, narrow the `events` filter to exclude `backend.capacity.warning`.

**Events delayed after restart**: On startup, the outbox worker begins draining within 2 seconds. Events persisted before a crash or restart are delivered on the next drain cycle.
