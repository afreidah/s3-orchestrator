---
description: "Interactive diagram of how a backend call is admitted against its monthly budgets, what it charges, and where those counters live."
title: "Usage Quotas"
linkTitle: "Usage Quotas"
weight: 10
---

How a backend call is admitted against its monthly budgets, what it charges, and where those counters live. **Hover over any component** for implementation details.

### How it works

Every call the orchestrator makes to a backend is admitted against that backend's monthly budgets before it goes out, and recorded after. Three dimensions are enforced: **egress bytes**, **ingress bytes**, and **requests**. The first two are scalars. Requests are not, because providers do not bill them as one.

#### Why requests are pooled

Providers group operations into billing classes with separate allowances, and they disagree about the grouping. GCS meters uploads, copies and listings from a small Class A allowance, reads from a Class B allowance ten times its size, and does not bill deletes at all. B2 inverts it: uploads and deletes are free, downloads and listings are charged from two different classes. OCI meters every request from one pooled allowance. IDrive e2 does not meter requests at all.

Charging every operation against a single `api_request_limit` forces a bad choice: set it to the strictest class and the rest of the headroom is wasted, set it to the loosest and the strict one is blown. On a GCS backend with a 5,000 upload allowance and a 50,000 read allowance, a single limit set for uploads takes the backend out of service once uploads are spent while nearly all of its read allowance is unused - and because the read path checks the same counter, the backend stops serving reads too.

A backend therefore declares **pools**: named budgets, each covering a set of operations.

```yaml
unmetered: [DeleteObject, DeleteObjects, AbortMultipartUpload]
request_limits:
  - name: class_a
    operations: [PutObject, CopyObject, ListObjects, ListObjectsV2]
    limit: 5000
  - name: class_b
    operations: [GetObject, HeadObject, GetParts]
    limit: 50000
```

#### The rules

1. **Pools are additive.** An operation charges every pool containing it and is admitted only when all of them have headroom. That is what lets a per-operation sub-cap sit inside an aggregate one.
2. **`"*"` covers everything not listed as `unmetered`.** A bare `api_request_limit` desugars to exactly one such pool, named `all`, so configs written before pools existed keep their old meaning.
3. **`limit: 0` counts without refusing.** Same convention the byte limits use.
4. **Unmetered means uncharged, not unrecorded.** A free operation still increments the backend's request total; it simply charges no pool. Not being billed for a call is not a reason to stop reporting that it happened.
5. **Deletes are never refused**, on any budget. Refusing one would leave an operator over a limit with no way back under it, and a client `DELETE` that returns without deleting is simply wrong. The one gated delete is the recovery check of a delete call's circuit breaker, which is admitted like any other check.

Because pools overlap, their counts do not sum to the request total and are not a decomposition of it.

#### What a refusal looks like

Admission is the same check on both paths; only the consequence differs. A **write** filters over-budget backends out of placement, so the object lands on another backend, or the client gets `507 Insufficient Storage` when none is eligible. A **read** skips over-budget copies and fails over to another; when every copy is on an over-budget backend the client gets `429 Too Many Requests` with `SlowDown`.

#### Where the counters live

A charge lands in the counter backend first - local atomics, or Redis when it is configured for multi-instance deployments - and is flushed to the metadata store (SQLite by default, or PostgreSQL) every 30 seconds (`usage_flush.interval`). With `usage_flush.adaptive_enabled: true` (default false), the interval drops to `usage_flush.fast_interval` (default 5s) once any budget passes `usage_flush.adaptive_threshold` (default 0.8). Admission reads **baseline + unflushed**: the DB figure the metrics collector last loaded, plus what this process has spent since. Enforcement is deliberately approximate, bounded by one flush interval of concurrent traffic, because exact enforcement would need a lock on every request.

{{< diagram >}}

<script>
window.s3oDiagram = {
  source: [
    'flowchart TD',
    '    CALL([Backend call<br>client request or worker pass]):::entry --> OPNAME[Name the Operation<br>s3op.Operation]:::process',
    '',
    '    OPNAME --> ISDEL{Delete?}:::decision',
    '    ISDEL -->|yes| UNGATED[Never Refused]:::success',
    '    ISDEL -->|no| RESOLVE[Resolve the Pools<br>this Operation Charges]:::process',
    '',
    '    RESOLVE --> BYTECHK{Egress and ingress<br>within limits?}:::filter',
    '    BYTECHK -->|no| REFUSE[Backend Refused]:::reject',
    '    BYTECHK -->|yes| UNMET{Listed as<br>unmetered?}:::decision',
    '    UNMET -->|yes| NOPOOL[Charged to<br>No Budget]:::process',
    '    UNMET -->|no| POOLCHK{Every charged pool<br>has headroom?}:::filter',
    '',
    '    POOLCHK -->|no| REFUSE',
    '    POOLCHK -->|yes| ADMIT[Admitted]:::success',
    '',
    '    REFUSE --> WRITEOUT[Write: next backend,<br>or 507 if none]:::reject',
    '    REFUSE --> READOUT[Read: next copy,<br>or 429 SlowDown]:::reject',
    '',
    '    ADMIT --> CALLBE[Call the Backend]:::storage',
    '    UNGATED --> CALLBE',
    '    NOPOOL --> CALLBE',
    '',
    '    CALLBE --> RECORD[Record: request total<br>plus each charged pool]:::process',
    '    RECORD --> COUNTER[Counter Backend<br>atomics or Redis]:::storage',
    '    COUNTER --> FLUSH[Usage Flusher<br>every 30s]:::process',
    '    FLUSH --> TABLES[backend_usage and<br>backend_request_usage]:::storage',
    '    TABLES --> BASELINE[Collector Reloads<br>Baselines]:::process',
    '    BASELINE --> BYTECHK',
    '    BASELINE --> POOLCHK',
  ].join('\n'),

  info: {
    CALL: {
      title: 'Backend Call',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Every call that reaches a provider passes through here, whatever asked for it: a client GET or PUT, a replication copy, a rebalance move, a scrub read, a reconcile listing page, one of the bulk rewrite passes, or a circuit breaker recovery check, which is admitted and charged as <code>HeadBucket</code> for the backend breaker and as the guarded call itself for a call breaker.</p><p>Client traffic and background work draw on the same budgets deliberately. A fleet-wide pass is the largest consumer of a metered backend, and one that spent freely while client reads were refused on the counter it ran up would be the wrong way round.</p><p><a href="../write-path/">Write path &rarr;</a> &middot; <a href="../read-path/">Read path &rarr;</a></p>'
    },
    OPNAME: {
      title: 'Name the Operation',
      badge: 'process', badgeText: 'classification',
      body: '<p>The charge carries which operation it is - <code>s3op.Operation</code>, a closed set of fourteen - rather than a count of calls.</p><p>A count cannot be priced. Providers bill an upload and a read from different allowances and some operations not at all, so "one API call" is not enough information to settle the charge. The same vocabulary is what config validates operation names against and what the per-operation metric labels use, so the three cannot drift apart.</p><p>Operations: <code>PutObject</code>, <code>GetObject</code>, <code>HeadObject</code>, <code>DeleteObject</code>, <code>DeleteObjects</code>, <code>CopyObject</code>, <code>ListObjects</code>, <code>ListObjectsV2</code>, <code>CreateMultipartUpload</code>, <code>UploadPart</code>, <code>CompleteMultipartUpload</code>, <code>AbortMultipartUpload</code>, <code>GetParts</code>, <code>HeadBucket</code>. A backend breaker\'s recovery check is admitted and charged as <code>HeadBucket</code>; a call breaker\'s check as the call it guards, such as <code>GetObject</code> for a one-byte read.</p>'
    },
    ISDEL: {
      title: 'Delete?',
      badge: 'decision', badgeText: 'gate exemption',
      body: '<p>Deletes are recorded but never gated, whatever any budget says.</p><p>A delete is the one operation that reduces what a backend holds. Refusing it over budget would trap an operator above a limit with no way back under it, and a client <code>DELETE</code> that returns success without removing the object is simply incorrect.</p><p>The same exemption covers the internal delete paths: cleanup-queue retries, drain purges, and lifecycle expiry.</p>'
    },
    UNGATED: {
      title: 'Never Refused',
      badge: 'success', badgeText: 'always admitted',
      body: '<p>The call proceeds regardless of remaining budget, and is still charged afterwards.</p><p>Whether it charges a pool is a separate question, answered by <code>unmetered</code>: on GCS, where deletes are free, an operator lists them there and the delete charges nothing but the request total. On a provider that does bill deletes, leaving them out of <code>unmetered</code> charges them normally - they just cannot be refused.</p>'
    },
    RESOLVE: {
      title: 'Resolve the Pools',
      badge: 'process', badgeText: 'lookup',
      body: '<p>Config compiles once at startup and on reload into a map from operation to the pools charging it, so this is a map lookup rather than matching names against pool definitions on every request.</p><p>An operation can belong to several pools. Pools are <b>additive</b>: it charges each of them and needs headroom in all of them, which is what lets a sub-cap on one operation sit inside an aggregate cap over everything.</p><p><a href="../../docs/backends/">Backend configuration &rarr;</a></p>'
    },
    UNMET: {
      title: 'Listed as unmetered?',
      badge: 'decision', badgeText: 'billing check',
      body: '<p><code>unmetered</code> names the operations the provider does not bill at all. They are removed from every pool, including a <code>"*"</code> wildcard, which would otherwise swallow them.</p><p>This is not the same as being ungated. An unmetered operation charges no budget; an ungated one cannot be refused. A delete on GCS is both; a delete on a provider that bills them is only the second.</p>'
    },
    NOPOOL: {
      title: 'Charged to No Budget',
      badge: 'process', badgeText: 'recorded, unbilled',
      body: '<p>The call still increments the backend\'s request total in <code>backend_usage.api_requests</code>. It simply charges no pool, so no budget moves toward refusing anything.</p><p>Recording it matters: the total is the honest answer to "how much did we use this backend", and an operator reading a request count should see the requests that were made, not the subset someone is billed for.</p><p>Free operations can be a large share of what a backend does: deletes can be close to half of a backend\'s operations, and charging them against a budget the provider does not apply to them exhausts it early.</p>'
    },
    POOLCHK: {
      title: 'Pool Headroom',
      badge: 'filter', badgeText: 'admission',
      body: '<p>For each pool the operation charges: <code>baseline + unflushed + proposed &le; limit</code>. All of them must pass. A pool with <code>limit: 0</code> is counted and never refuses.</p><p>Effective usage is the DB baseline the metrics collector last loaded plus what this process has spent since the last flush. Enforcement is approximate by design - concurrent requests can all pass and collectively overshoot by up to one flush interval - because exact enforcement would need a lock on every request.</p><p>This is where a class split earns its keep: an upload budget can be spent while the read budget is untouched, and reads keep being served.</p><p class="ac-metric">Metrics: s3o_usage_pool_requests{backend,pool}, s3o_usage_pool_limit{backend,pool}</p>'
    },
    BYTECHK: {
      title: 'Byte Limits',
      badge: 'filter', badgeText: 'admission',
      body: '<p><code>egress_byte_limit</code> is checked first, then <code>ingress_byte_limit</code>, both against the same baseline-plus-unflushed view the pools use. The pool check runs after both pass.</p><p>On a GET the size is known only once the backend has answered, so the egress check for the bytes runs after the call. A read refused there has already been charged one request.</p><p>Write admission also drops a backend whose per-backend <code>max_object_size</code> is smaller than the upload.</p><p>These stay scalar because providers do not class bytes: a gigabyte out is a gigabyte out, whichever call moved it. Only requests needed pooling.</p><p>The size charged is the size that will actually cross the link - the encoded bytes for a compressed object, the envelope for an encrypted one - not the logical size the client sees.</p>'
    },
    REFUSE: {
      title: 'Backend Refused',
      badge: 'reject', badgeText: 'over budget',
      body: '<p>This backend cannot absorb the operation. A refusal before the call charges nothing, because nothing was sent. A read refused on egress after the backend answered is charged one request and no bytes.</p><p>The refusal is per backend, not per request: a fleet is only out of budget when every eligible backend is, which is what makes overflow to another provider the normal response to one hitting its ceiling.</p><p class="ac-metric">Metric: s3o_usage_limit_rejections_total{operation,limit_type}</p>'
    },
    WRITEOUT: {
      title: 'Write: Overflow or 507',
      badge: 'reject', badgeText: 'write path',
      body: '<p>Placement starts from the write targets: backends that accept writes and serve both <code>PutObject</code> and <code>GetObject</code>. Over-budget ones are then filtered out before a target is picked, so a write simply lands somewhere else.</p><p>When no backend is eligible the client gets <code>507 Insufficient Storage</code>, which can therefore mean no budget left or no healthy write target, and the pre-flight <code>CanAcceptWrite</code> check makes that answer arrive before the body is transmitted rather than after.</p><p><a href="../admission-control/">Admission control &rarr;</a></p>'
    },
    READOUT: {
      title: 'Read: Failover or 429',
      badge: 'reject', badgeText: 'read path',
      body: '<p>A read skips the copy on an over-budget backend and tries the next one holding the object.</p><p>When every copy is on an over-budget backend the read fails with <code>ErrUsageLimitExceeded</code>, which the handler returns as <code>429 Too Many Requests</code> with the <code>SlowDown</code> S3 code - a retryable answer, since the budget resets.</p><p>This is why a single undifferentiated limit was worse than it looked: exhausting it did not merely stop writes, it made a backend unreadable as a serving copy.</p>'
    },
    ADMIT: {
      title: 'Admitted',
      badge: 'success', badgeText: 'within budget',
      body: '<p>Every pool the operation charges has room, and both byte dimensions do too.</p><p>Admission and accounting live on one type (<code>accounting.Recorder</code>) so a caller holding it can always ask before spending. With recording and asking on separate surfaces, a path could record what it spent without asking first, which keeps the counters truthful while the budget is spent unchecked.</p>'
    },
    CALLBE: {
      title: 'Call the Backend',
      badge: 'storage', badgeText: 'S3 API call',
      body: '<p>The HTTP request goes out to the provider.</p><p>It is charged whatever the outcome. A call that failed still reached the provider and still counts against the allowance, so recording only successes would drift the ledger away from the bill.</p>'
    },
    RECORD: {
      title: 'Record the Charge',
      badge: 'process', badgeText: 'accounting',
      body: '<p>One charge updates both views: <code>+1</code> on the backend\'s request total, and <code>+1</code> on every pool the operation belongs to. A rewrite pass that reads and writes one object charges a read pool and a write pool, not two of whichever pool a scalar count would have landed in.</p><p>Bytes are added on the same call: egress for a successful GET-like operation, ingress for a PUT-like one.</p><p>A backend-to-backend copy (replication, rebalance, drain, a rewrite pass) is admitted against both backends on the copy\'s expected size and charged inside the copy itself: the read at the bytes the source served, the write at the bytes the destination received. A rewrite that changes an object\'s size is therefore charged the new size on the write. A leg that fails is charged its API call and no bytes.</p><p>Because pools overlap, their counts do not sum to the request total and must never be presented as if they did.</p>'
    },
    COUNTER: {
      title: 'Counter Backend',
      badge: 'storage', badgeText: 'in-memory or Redis',
      body: '<p>Charges land in memory first: per-backend atomics by default, or Redis when configured, which shares counters across instances so a fleet enforces one budget rather than one per process.</p><p>All of a backend\'s unflushed counters live in one Redis hash, each field prefixed with its hourly bucket and workload: the totals, and a <code>pool:</code> field for each pool charged. A charge is one transaction on that hash, and a limit check is one read of it, summing the buckets from the start of the backend\'s cycle.</p><p>Redis failures fall back to local counters, kept per bucket. Each instance flushes those itself; nothing is replayed into Redis.</p>'
    },
    FLUSH: {
      title: 'Usage Flusher',
      badge: 'process', badgeText: 'every 30s',
      body: '<p>Takes the counters as one batch per backend, split into hourly buckets, and writes each batch&#39;s totals and pool counts to the metadata store (SQLite by default, or PostgreSQL) in one transaction. Runs every <code>usage_flush.interval</code> (default 30s) on every instance.</p><p>With <code>usage_flush.adaptive_enabled: true</code> (default false), the interval shortens to <code>fast_interval</code> (default 5s) when any budget - a byte limit or a request pool - passes <code>adaptive_threshold</code> (default 0.8), because that is when enforcement accuracy starts to matter.</p><p>With Redis, a flush renames each live hash to a staged key under a fresh flush id and deletes it only after the database commit, which records the id with the deltas. A staged hash left by a crash or a lost reply is flushed again by any instance and applied once. No advisory lock is taken.</p><p><a href="../background-services/">Background services &rarr;</a></p>'
    },
    TABLES: {
      title: 'Metadata Store Counters',
      badge: 'storage', badgeText: 'hourly rows',
      body: '<p><code>backend_usage(backend_name, bucket_start, workload, api_requests, egress_bytes, ingress_bytes)</code> holds the totals; <code>backend_request_usage(backend_name, bucket_start, workload, pool, requests)</code> holds one row per budget.</p><p>Every charge carries the workload on its request context (<code>client</code>, <code>scrub</code>, <code>replication</code>, ...), so each row says what caused it; a backend\'s usage is the sum of its workloads, and that sum is what limits are judged against.</p><p>Both are keyed by the UTC hour a charge was made in. A backend\'s cycle is the sum of its buckets from the cycle\'s start, set by <code>usage_reset_day</code> and <code>usage_reset_timezone</code>, so a cycle rolls over on its own with no reset job. Buckets older than 100 days are pruned on flush. Both use additive <code>ON CONFLICT</code> upserts, so several instances flushing at once converge instead of overwriting each other.</p><p>Bytes stay columnar because their dimensions are fixed; pool counts are keyed because their names come from config and change with it.</p><p><a href="../database-schema/">Database schema &rarr;</a></p>'
    },
    BASELINE: {
      title: 'Reload Baselines',
      badge: 'process', badgeText: 'closes the loop',
      body: '<p>The metrics collector reads both tables from each backend\'s cycle start and seeds the tracker\'s baselines, which is what makes a restarted process pick up mid-cycle rather than starting from zero.</p><p>Both halves are loaded together: seeding the totals without the pool counts would admit work against a budget already spent.</p><p>Baselines reset at each backend\'s cycle reset and when a backend is removed, since its rows are gone. Draining a backend leaves them in place.</p>'
    }
  }
};
</script>

## Legend

| Node | Meaning |
|------|---------|
| <span class="dg-swatch dg-entry"></span> | Entry point |
| <span class="dg-swatch dg-filter"></span> | Budget check |
| <span class="dg-swatch dg-decision"></span> | Decision / branch |
| <span class="dg-swatch dg-process"></span> | Processing step |
| <span class="dg-swatch dg-storage"></span> | Counter storage |
| <span class="dg-swatch dg-success"></span> | Admitted |
| <span class="dg-swatch dg-reject"></span> | Refused |

## See also

- [Backend configuration](../../docs/backends/) - `request_limits`, `unmetered`, and the operation vocabulary
- [Maximizing free tiers](../../guides/maximizing-free-tiers/) - a worked GCS class split
- [Write path](../write-path/) and [read path](../read-path/) - where admission sits in each
- [Admission control](../admission-control/) - the storage-capacity pre-flight this runs alongside
- [Background services](../background-services/) - the usage flusher that drains these counters
- [Database schema](../database-schema/) - `backend_usage` and `backend_request_usage`
