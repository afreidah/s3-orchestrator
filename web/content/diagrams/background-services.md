---
description: "Interactive diagram of how the periodic workers coordinate to maintain storage health, enforce replication, and persist counters."
title: "Background Services Flow"
linkTitle: "Background Services Flow"
weight: 7
---

Coordination of periodic background workers that maintain storage health, enforce replication, and persist counters. **Hover over any component** for implementation details.

<style>
  #ac-diagram { margin: 1rem 0; }
  #ac-tooltip {
    position: fixed; z-index: 9999; pointer-events: none;
    max-width: 380px; padding: 0.7rem 0.85rem;
    background: #161b22; border: 1px solid #30363d; border-radius: 6px;
    box-shadow: 0 4px 16px rgba(0,0,0,0.4); display: none;
  }
  #ac-tooltip h3 { color: #2a9d73; font-size: 0.85rem; margin: 0 0 0.25rem 0; }
  #ac-tooltip .ac-badge {
    display: inline-block; padding: 1px 7px; border-radius: 4px;
    font-size: 0.6rem; font-weight: 600; margin-bottom: 0.4rem; text-transform: uppercase;
  }
  .ac-badge-entry { background: #1a7a5a22; color: #34b882; border: 1px solid #34b88255; }
  .ac-badge-filter { background: #6b5b2e22; color: #c4a35a; border: 1px solid #c4a35a55; }
  .ac-badge-decision { background: #2a9d7322; color: #2a9d73; border: 1px solid #2a9d7355; }
  .ac-badge-process { background: #2d7d6a22; color: #5ec9a0; border: 1px solid #5ec9a055; }
  .ac-badge-storage { background: #1a3a3022; color: #4aaa8a; border: 1px solid #4aaa8a55; }
  .ac-badge-success { background: #1a7a5a22; color: #34b882; border: 1px solid #34b88255; }
  .ac-badge-reject { background: #8b3a3a22; color: #d4a0a0; border: 1px solid #d4a0a055; }
  .ac-badge-cleanup { background: #4a556822; color: #8a9aa8; border: 1px solid #8a9aa855; }
  #ac-tooltip p { font-size: 0.75rem; line-height: 1.4; color: #c9d1d9; margin-bottom: 0.35rem; }
  #ac-tooltip code { background: #21262d; padding: 1px 4px; border-radius: 3px; font-size: 0.7rem; color: #4aaa8a; }
  #ac-tooltip .ac-metric { color: #a7d5c1; font-style: italic; font-size: 0.7rem; }
  #ac-diagram .node, #ac-diagram .edgePath, #ac-diagram .edgeLabel { transition: opacity 0.15s, filter 0.15s; }
  #ac-diagram svg.highlighting .node, #ac-diagram svg.highlighting .edgePath, #ac-diagram svg.highlighting .edgeLabel { opacity: 0.12; }
  #ac-diagram svg.highlighting .node.highlight, #ac-diagram svg.highlighting .edgePath.highlight, #ac-diagram svg.highlighting .edgeLabel.highlight { opacity: 1; filter: drop-shadow(0 0 6px rgba(42,157,115,0.5)); }
  #ac-diagram .node { cursor: pointer; }
</style>

<div id="ac-diagram"></div>
<div id="ac-tooltip"></div>

<script src="https://cdn.jsdelivr.net/npm/mermaid@11.8.0/dist/mermaid.min.js"></script>
<script>
(function() {
  var diagramSrc = [
    'flowchart LR',
    '    SCHED([Lifecycle<br>Manager]):::entry --> REPL[Replicator]:::process',
    '    SCHED --> REBAL[Rebalancer]:::process',
    '    SCHED --> OVERREP[Over-Replication<br>Cleaner]:::process',
    '    SCHED --> DRAINER[Backend<br>Drain]:::process',
    '    SCHED --> LIFECYCLE[Lifecycle<br>Expiration]:::process',
    '    SCHED --> MPCLEAN[Multipart<br>Cleanup]:::process',
    '    SCHED --> FLUSH[Usage<br>Flusher]:::filter',
    '    SCHED --> FLEET[Fleet<br>Snapshot]:::filter',
    '    SCHED --> CQWORKER[Cleanup Queue<br>Worker]:::cleanup',
    '    SCHED --> PENDREAP[Pending<br>Reaper]:::process',
    '    SCHED --> RECONCILE[Orphan<br>Reconciler]:::process',
    '    SCHED --> SCRUBBER[Integrity<br>Scrubber]:::process',
    '    SCHED --> CBWATCH[CB<br>Watchdog]:::filter',
    '    SCHED --> NOTIFY[Notification<br>Drainer]:::process',
    '    SCHED --> PROVWATCH[Provisioning<br>Watch]:::filter',
    '    SCHED --> FLIGHTREC[Flight<br>Recorder]:::filter',
    '',
    '    REPL -->|copy to| S3[S3<br>Backends]:::storage',
    '    REBAL -->|move between| S3',
    '    OVERREP -->|delete from| S3',
    '    DRAINER -->|move off| S3',
    '    DRAINER -->|advance drain record| PG',
    '    LIFECYCLE -->|delete from| S3',
    '    MPCLEAN -->|abort on| S3',
    '    CQWORKER -->|retry on| S3',
    '    RECONCILE -->|list + import| S3',
    '    SCRUBBER -->|read + verify| S3',
    '    SCRUBBER -->|on corruption| CQ',
    '',
    '    REPL -->|on failure| CQ{{Cleanup<br>Queue}}:::cleanup',
    '    REBAL -->|on failure| CQ',
    '    OVERREP -->|on failure| CQ',
    '    CQWORKER -->|fetch items| CQ',
    '    PENDREAP -->|HEAD probe| S3',
    '    PENDREAP -->|read + resolve| PG',
    '',
    '    FLUSH -->|persist| PG[(PostgreSQL)]:::storage',
    '    FLEET -->|ledger + backlog counts| PG',
    '    NOTIFY -->|drain outbox| PG',
    '    PROVWATCH -->|reload provisioning| PG',
    '',
    '    classDef entry fill:#1a7a5a,stroke:#1a7a5a,color:#fff,font-weight:bold',
    '    classDef filter fill:#6b5b2e,stroke:#c4a35a,color:#fff',
    '    classDef decision fill:#1e2a26,stroke:#2a9d73,color:#e6edf3,font-size:11px',
    '    classDef process fill:#2d7d6a,stroke:#5ec9a0,color:#fff',
    '    classDef storage fill:#1a3a30,stroke:#4aaa8a,color:#c9d1d9',
    '    classDef success fill:#1a7a5a,stroke:#34b882,color:#fff,font-weight:bold',
    '    classDef reject fill:#8b3a3a,stroke:#d4a0a0,color:#fff,font-weight:bold',
    '    classDef cleanup fill:#222a26,stroke:#8a9aa8,color:#e6edf3'
  ].join('\n');

  mermaid.initialize({
    startOnLoad: false,
    theme: 'base',
    themeVariables: {
      darkMode: true,
      background: '#191c23',
      fontFamily: 'Inter, ui-sans-serif, system-ui, sans-serif',
      fontSize: '15px',
      primaryColor: '#26332f',
      primaryTextColor: '#f8fafc',
      primaryBorderColor: '#2a9d73',
      secondaryColor: '#3a2e20',
      secondaryTextColor: '#e8dfd0',
      secondaryBorderColor: '#c4a35a',
      tertiaryColor: '#20262d',
      tertiaryTextColor: '#e8dfd0',
      tertiaryBorderColor: '#4aaa8a',
      lineColor: '#7f8b86',
      edgeLabelBackground: '#191c23',
      clusterBkg: '#1d2229',
      clusterBorder: '#39443f'
    },
    flowchart: { nodeSpacing: 80, rankSpacing: 160, curve: 'linear', padding: 12, diagramPadding: 16, useMaxWidth: true, htmlLabels: true }
  });

  mermaid.render('bg-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('ac-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    SCHED: {
      title: 'Lifecycle Manager (Scheduler)',
      badge: 'entry', badgeText: 'scheduler',
      body: '<p>Central service orchestrator from <code>internal/lifecycle</code>. Launches all background workers as supervised goroutines.</p><p>Each service implements <code>lifecycle.Runner</code> with a <code>Run(ctx)</code> method. Most workers use <code>tickrunner.Service</code> (<code>internal/lifecycle/tickrunner</code>), which runs each tick behind a PostgreSQL advisory lock (<code>pg_try_advisory_lock</code>) so only one instance does the work.</p><p>Services are registered in <code>internal/di/lifecycle.go</code>. Most constructors live beside their worker: <code>internal/worker/*_service.go</code>, <code>internal/proxy/multipart/cleanup_service.go</code>, <code>internal/breaker/watchdog.go</code>. Hot-reloaded config is held in a <code>syncutil.AtomicConfig</code> on the worker or manager that reads it (<code>worker.Replicator</code>, <code>worker.Scrubber</code>, <code>expiry.Manager</code>).</p><p><b>Start-up stagger</b>: a <code>tickrunner.Service</code> waits a random delay of up to half its interval before it starts its ticker, so instances do not all contend for the lock at once. Its first tick comes one interval after that delay. A service with a startup pass, such as the replicator, runs that pass before the delay.</p><p><b>Health tracking</b>: every <code>tickrunner.Service</code> records per-tick state (last success, last failure, last error, consecutive failures). It is exposed through <code>GET /admin/api/workers</code> and as Prometheus metrics, so operators can alert on stalled or failing workers without reading logs.</p><p class="ac-metric">Per-service generic metrics: s3o_worker_ticks_total{service,result=success|error|skipped}, s3o_worker_last_success_timestamp_seconds{service}, s3o_worker_consecutive_failures{service}</p>'
    },
    REPL: {
      title: 'Replicator',
      badge: 'process', badgeText: 'every 5 min',
      body: '<p><code>Replicator.Replicate()</code> creates additional copies of under-replicated objects to reach the configured replication factor.</p><p><b>Interval</b>: default 5 minutes (configurable via <code>replication.worker_interval</code>).<br><b>Advisory lock</b>: <code>LockReplicator = 1002</code>.<br><b>Batch size</b>: configurable, queries <code>GetUnderReplicatedObjects()</code>.<br><b>Concurrency</b>: parallel via <code>workerpool.Run()</code>.</p><p>Runs a <b>startup pass</b> immediately on boot for catch-up. Excludes backends unhealthy longer than <code>unhealthy_threshold</code>. Uses <code>StreamCopy()</code> for zero-buffer transfer, reading the source at its row\'s storage key and writing the copy to a path minted for it. Conditional <code>RecordReplica()</code> DB insert guards against concurrent overwrites/deletes.</p><p>On copy failure, stale source, or a copy superseded by a write that committed on the same backend first: the copy\'s own path is cleaned via <code>DeleteOrEnqueue()</code> with reason <code>replication_orphan</code>. Nothing else at that key is touched.</p><p>Not the only way a copy is made. With <code>write_path.parallel_copies</code> on the write places its own, leaving this worker with repair: objects a write could not place, health-triggered replacements, multipart uploads, and copies discarded because a newer write took the key mid-upload. The scan is unchanged; there is simply less for it to find.</p><p class="ac-metric">Metrics: s3o_replication_copies_created_total, s3o_replication_runs_total{status}, s3o_replication_errors_total, s3o_replication_duration_seconds, s3o_replication_pending, s3o_replication_health_copies_total &bull; write-placed: s3o_replication_write_copies_committed, s3o_replication_write_copies_total{outcome}, s3o_detached_uploads_depth, s3o_replication_write_fanout_skipped_total</p>'
    },
    REBAL: {
      title: 'Rebalancer',
      badge: 'process', badgeText: 'every 6 hrs',
      body: '<p><code>Rebalancer.Rebalance()</code> moves objects between backends to optimize space distribution.</p><p><b>Interval</b>: default 6 hours (configurable via <code>rebalance.interval</code>).<br><b>Advisory lock</b>: <code>LockRebalancer = 1001</code>.<br><b>Guard</b>: skips if disabled or utilization spread &lt; <code>threshold</code>.</p><p><b>Strategies</b>:<br>&bull; <code>spread</code>: equalizes utilization ratios (most over-target sources &rarr; most under-target destinations)<br>&bull; <code>pack</code>: consolidates onto most-full backends, pulling from least-full</p><p>Each move: <code>StreamCopy()</code> to a fresh path on the destination &rarr; <code>MoveObjectLocation()</code> (atomic CAS, recording the new path) &rarr; delete source at its old path. On DB failure: destination orphan cleaned via <code>DeleteOrEnqueue()</code> with reason <code>rebalance_orphan</code>. Source delete failures use reason <code>rebalance_source_delete</code>.</p><p class="ac-metric">Metrics: s3o_rebalance_objects_moved_total{strategy,status}, s3o_rebalance_bytes_moved_total{strategy}, s3o_rebalance_runs_total{strategy,status}, s3o_rebalance_duration_seconds{strategy}, s3o_rebalance_skipped_total{reason}, s3o_rebalance_pending</p>'
    },
    OVERREP: {
      title: 'Over-Replication Cleaner',
      badge: 'process', badgeText: 'every 5 min',
      body: '<p><code>OverReplicationCleaner.Clean()</code> removes surplus copies that exceed the target replication factor.</p><p><b>Interval</b>: default 5 minutes (configurable via <code>replication.worker_interval</code>).<br><b>Advisory lock</b>: <code>LockOverReplication = 1008</code>.<br><b>Guard</b>: only runs when <code>factor > 1</code>.</p><p>Queries <code>GetOverReplicatedObjects()</code>, groups by key, then scores each copy:<br>&bull; Draining backend: score 0 (remove first)<br>&bull; Circuit-broken backend: score 1<br>&bull; Healthy backend: 2 + (1 - utilization ratio), range [2..3]</p><p>Lowest-scoring copies removed first. Uses <code>RemoveExcessCopy()</code> with <code>FOR UPDATE</code> row lock to prevent races with concurrent replicator/rebalancer. Physical delete via <code>DeleteOrEnqueue()</code> with reason <code>over_replication</code>.</p><p class="ac-metric">Metrics: s3o_over_replication_removed_total, s3o_over_replication_runs_total{status}, s3o_over_replication_errors_total, s3o_over_replication_pending, s3o_over_replication_duration_seconds, s3o_over_replication_key_preserved_total</p>'
    },
    DRAINER: {
      title: 'Backend Drain',
      badge: 'process', badgeText: 'every 10 s',
      body: '<p><code>Drainer.Drain()</code> works every backend whose drain record is in progress.</p><p><b>Interval</b>: 10 seconds, which only decides how soon a new or resumed drain is picked up; a pass works each drain until it finishes or stalls.<br><b>Advisory lock</b>: <code>LockDrain = 1006</code>.<br><b>Guard</b>: does nothing while no drain is in progress.</p><p>Each pass aborts the backend\'s open multipart uploads, then lists its objects 100 at a time, smallest first, by a <code>(size_bytes, object_key)</code> cursor. Each page looks up every object\'s copies with one <code>GetObjectBackendsForKeys()</code> call. An object another backend also holds loses only its draining copy, through a conditional <code>RemoveExcessCopy()</code> that re-reads the copies under the key lock and refuses to drop the only copy holding the object\'s usable encryption key; the rest are moved with <code>MoveObject()</code> to a fresh path on the least-utilized eligible backend. An object that fails is passed over for the rest of the pass rather than listed again. Between pages it re-reads the record, so a cancel stops the pass. Once the listing is walked, <code>CompleteDrain()</code> marks it drained only if no managed rows, in-flight intents, or multipart uploads remain, so anything that failed keeps the drain open for the next tick; a hard error marks the drain failed instead.</p><p class="ac-metric">Metrics: s3o_drain_active, s3o_drain_objects_moved_total, s3o_drain_bytes_moved_total &bull; Events: backend.drain.completed, backend.drain.failed</p>'
    },
    LIFECYCLE: {
      title: 'Lifecycle Expiration',
      badge: 'process', badgeText: 'every 1 hr',
      body: '<p><code>expiry.Manager.ProcessRules()</code> evaluates TTL-based lifecycle rules and deletes expired objects.</p><p><b>Interval</b>: 1 hour.<br><b>Advisory lock</b>: <code>LockLifecycle = 1005</code>.<br><b>Guard</b>: only runs when lifecycle rules are configured.<br><b>Batch size</b>: <code>lifecycle.batch_size</code>, default 100 per rule.</p><p>For each rule: computes <code>cutoff = now - expiration_days * 24h</code>, pages through <code>ListExpiredObjects()</code> with the rule prefix, tags, cutoff and batch size by object-key cursor, so an object that fails to delete is passed over rather than listed again, then calls the standard <code>DeleteObject()</code> path (quota decrement, cache invalidation, cleanup queue on failure).</p><p>Audit event: <code>lifecycle.delete</code> with key, prefix, expiration_days.</p><p class="ac-metric">Metrics: s3o_lifecycle_deleted_total, s3o_lifecycle_failed_total, s3o_lifecycle_runs_total{status=success|partial|error}</p>'
    },
    MPCLEAN: {
      title: 'Multipart Cleanup',
      badge: 'process', badgeText: 'every 1 hr',
      body: '<p><code>multipart.Manager.CleanupStaleMultipartUploads()</code> aborts multipart uploads older than the stale threshold.</p><p><b>Interval</b>: 1 hour.<br><b>Advisory lock</b>: <code>LockMultipartCleanup = 1004</code>.<br><b>Stale threshold</b>: <code>cleanup_queue.multipart_stale_timeout</code>, default 24 hours.</p><p>Pages through <code>ScanMultipartUploads()</code> with a <code>CreatedBefore</code> filter for uploads older than the threshold, by upload-ID cursor. The drain worker uses the same query with a <code>Backend</code> filter. Each stale upload is aborted: its uploaded parts are deleted from the backend in one batched request through <code>DeleteAllOrEnqueue()</code> with reason <code>abort_part_cleanup</code>, and its DB records are removed.</p><p>Audit event: <code>storage.MultipartCleanup</code> with cleaned count and total stale count.</p>'
    },
    FLUSH: {
      title: 'Usage Flusher',
      badge: 'filter', badgeText: 'every 30s (adaptive)',
      body: '<p><code>UsageTracker.FlushUsage()</code> reads and resets the usage counters, then writes the accumulated deltas (API requests, egress, ingress) to <code>backend_usage</code> and the per-pool request deltas to <code>backend_request_usage</code> through <code>FlushPoolDeltas</code>.</p><p><b>Interval</b>: default 30 seconds (<code>usage_flush.interval</code>).<br><b>Adaptive mode</b>: only when <code>usage_flush.adaptive_enabled</code> is true. When any backend passes <code>adaptive_threshold</code> (default 0.8) of its usage limit, the interval shortens to <code>fast_interval</code> (default 5s).<br><b>Advisory lock</b>: <code>LockUsageFlush = 1007</code>, taken only when Redis counters are configured, whatever their health, so a recovery part-way through a flush cannot double-count.</p><p><b>Each tick</b>:<br>&bull; Every instance runs <code>FlushQuota()</code>, which reloads the quota baselines placement ranks against and writes nothing.<br>&bull; With Redis: the holder of lock 1007 runs <code>FlushUsage()</code>. Every instance then runs <code>LoadFleetMetrics()</code> and <code>LoadWorkerGauges()</code>, applying the fleet snapshot and worker gauges last published. The flush never computes the fleet snapshot; the Fleet Snapshot service does.<br>&bull; Without Redis: no lock is taken, and every instance runs <code>FlushUsage()</code>.<br>&bull; Every instance then runs <code>RefreshUsageBaselines()</code>, so its limit checks include what was just written, and reloads its cached drain states from <code>backend_drains</code>, so a drain started on another instance stops being offered as a write target within one tick.</p><p>Counters are keyed by calendar month (<code>YYYY-MM</code>) for automatic period rollover. On a DB error the deltas are added back so they are not lost.</p><p class="ac-metric">Metrics: s3o_usage_api_requests{backend}, s3o_usage_egress_bytes{backend}, s3o_usage_ingress_bytes{backend}, s3o_usage_pool_requests{backend,pool}</p>'
    },
    FLEET: {
      title: 'Fleet Snapshot',
      badge: 'filter', badgeText: 'every 60 sec',
      body: '<p><code>Collector.RefreshFleetIfStale()</code> computes the fleet-wide figures, applies them to the gauges, and with Redis publishes them for the other instances.</p><p><b>Interval</b>: <code>telemetry.metrics.fleet_interval</code>, default 60 seconds, minimum 10.<br><b>Advisory lock</b>: <code>LockFleetSnapshot = 1013</code>.<br><b>Runs in</b>: every mode, so API-only instances serve the same figures.</p><p><b>Each tick</b>: reads the published snapshot first. One younger than half the interval is applied as it is, so however many instances take the lock in turn, the ledger is scanned once per interval. Otherwise it computes a new one from <code>GetQuotaStats()</code>, <code>LedgerStats()</code> (one grouped pass over <code>object_locations</code> giving per-backend objects, unhashed, plaintext, unreadable, compression and verification-coverage figures), <code>GetActiveMultipartCounts()</code> and <code>CountReplicationBacklog()</code> (exact under- and over-replicated counts in one pass).</p><p>The snapshot is published with a TTL of three intervals. The dashboard, <code>/ui/api/dashboard</code> and <code>/admin/api/status</code> read it rather than querying the ledger per request, computing one live only when none exists yet. A rebalance, replication, over-replication, reconcile or reload pass that changes something recomputes it immediately.</p><p class="ac-metric">Metrics: s3o_quota_bytes_used{backend}, s3o_quota_bytes_limit{backend}, s3o_quota_bytes_available{backend}, s3o_quota_orphan_bytes{backend}, s3o_objects_count{backend}, s3o_active_multipart_uploads{backend}, s3o_encryption_plaintext_copies, s3o_unreadable_copies, s3o_replication_pending, s3o_over_replication_pending</p>'
    },
    CQWORKER: {
      title: 'Cleanup Queue Worker',
      badge: 'cleanup', badgeText: 'every 1 min',
      body: '<p><code>CleanupWorker.ProcessCleanupQueue()</code> retries failed object deletions from the <code>cleanup_queue</code> table. Each row is deleted at its <code>storage_key</code>, the path of the one write\'s bytes it names; <code>object_key</code> is carried only so an operator can tell which object an orphan belonged to. A tick deletes its claimed rows with one batched request per backend, under one admission slot each, then completes, retries or graduates each row on its own result.</p><p><b>Interval</b>: 1 minute.<br><b>Advisory lock</b>: <code>LockCleanupQueue = 1003</code>.<br><b>Batch size</b>: claims 50 rows at a time, earliest <code>next_retry</code> first, for up to 20 batches per tick. It stops early on a batch shorter than 50, since the queue has run dry, or on a batch that settles nothing, since the backends are refusing.<br><b>Concurrency</b>: configurable (default 10).<br><b>Claim grace period</b>: configurable via <code>cleanup_queue.claim_grace_period</code> (default 5m).</p><p><b>Per-row claim pattern</b>: each tick calls <code>ClaimPendingCleanups</code>, a CTE that selects candidate rows <code>FOR UPDATE SKIP LOCKED</code> and then runs <code>UPDATE ... FROM</code> the candidate set to atomically reserve a batch of rows and stamp them with the calling instance\'s identifier (<code>claimed_at</code>, <code>claimed_by</code>). Two ticks running concurrently across instances always return disjoint row sets, so connection death and rolling-deploy overlap cannot let two workers process the same row. A claim older than the grace period is reclaimable so a worker that died mid-process does not leave the row stuck; reclaims emit <code>s3o_cleanup_queue_stale_claims_recovered_total</code> and a <code>cleanup_queue.claim_recovered</code> audit event.</p><p><b>Backoff</b>: exponential <code>min(1m * 2^attempts, 24h)</code>. Scheduling a retry clears the row\'s claim so it is immediately re-eligible for the next tick.<br><b>Max attempts</b>: 10. On the tenth consecutive failure the row is graduated to <code>cleanup_dlq</code> via <code>core.MoveCleanupToDLQ</code> so it surfaces for operator action; <code>orphan_bytes</code> is intentionally NOT decremented because the backend object is still on disk.</p><p>Rows are written by <code>Coordinator.EnqueueCleanup()</code>, usually through <code>DeleteOrEnqueue()</code> or <code>DeleteAllOrEnqueue()</code>, at every failure site: overwrites, deletes, multipart part cleanup, the rebalancer, replicator, over-replication cleaner, drain, scrubber, read-path integrity checks and the pending reaper. See the Cleanup Queue node for the reasons. A failed enqueue is counted in <code>s3o_cleanup_enqueue_failures_total</code> and audited as <code>storage.OrphanEnqueueFailed</code>.</p><p><b>Outcomes</b>: on success, <code>CompleteCleanupItem</code> deletes the row and decrements <code>orphan_bytes</code> for the backing backend in a single atomic CTE, so a worker crash cannot leave the counter inconsistent and a re-claim of an already-deleted row is a no-op. A DELETE that returns 404 also completes the row, with status <code>success_absent</code> and audit event <code>cleanup_queue.already_absent</code>. A row whose backend is no longer configured is completed without a delete.</p><p class="ac-metric">Metrics: s3o_cleanup_queue_enqueued_total{reason}, s3o_cleanup_queue_processed_total{status=success|success_absent|retry|exhausted}, s3o_cleanup_queue_depth, s3o_cleanup_queue_stale_claims_recovered_total{backend}, s3o_cleanup_enqueue_failures_total{backend,reason,stage}, s3o_cleanup_dlq_enqueued_total{backend}, s3o_cleanup_dlq_depth</p>'
    },
    PENDREAP: {
      title: 'Pending Reaper',
      badge: 'process', badgeText: 'every 1 min',
      body: '<p><code>PendingReaper.ProcessPendingQueue()</code> resolves unresolved <code>pending_objects</code> rows from the write-path PUT-before-COMMIT pattern. If the orchestrator died between the backend PUT and the metadata commit, this worker is what makes the orphan recoverable.</p><p><b>Interval</b>: configurable via <code>write_path.pending_pattern.reaper_tick</code> (default 1 minute).<br><b>Min age</b>: configurable via <code>write_path.pending_pattern.min_age</code> (default 5 minutes). Only intents older than this are eligible, so the reaper does not race a PUT that is still in flight.<br><b>Batch size</b>: configurable via <code>write_path.pending_pattern.batch_size</code> (default 50).<br><b>Concurrency</b>: 4 (hard-coded in <code>NewPendingReaper</code>).<br><b>Advisory lock</b>: <code>LockPendingReaper = 1011</code>. The notification drainer currently takes the same lock 1011, so each skips a tick while the other holds it.</p><p>The stale scan, <code>GetStalePendingObjects</code>, is a plain SELECT. Promotion re-reads the row with a plain <code>FOR UPDATE</code> through <code>LockPendingForUpdate</code>, so a row already resolved elsewhere is a no-op.</p><p>For each stale intent: an intent whose backend is no longer configured is dropped. One on a backend with an open circuit breaker is left for the next tick. Otherwise the reaper HEADs the backend at the intent\'s storage key. HEAD 200 &rarr; promote (commit the intent as a real <code>object_locations</code> row, or settle a companion copy as kept or discarded). HEAD 404 &rarr; drop (the backend never received the bytes, so no orphan exists). Any other error leaves the intent for the next tick.</p><p>Audit events: <code>pending_reaper.promoted</code>, <code>pending_reaper.dropped</code>, <code>pending_reaper.superseded</code>, <code>pending_reaper.companion_kept</code>, <code>pending_reaper.companion_discarded</code>.</p><p class="ac-metric">Metrics: s3o_pending_intents_enqueued_total, s3o_pending_intents_resolved_total{status=committed|promoted|dropped|superseded|companion_kept|companion_discarded|ambiguous|already_resolved}, s3o_pending_intents_depth</p>'
    },
    SCRUBBER: {
      title: 'Integrity Scrubber',
      badge: 'process', badgeText: 'configurable (default 6h)',
      body: '<p><code>Scrubber.Scrub()</code> takes the copies least recently verified, computes their SHA-256 hash, and compares against the stored <code>content_hash</code>.</p><p><b>Ordering</b>: <code>COALESCE(last_scrubbed_at, created_at)</code>, so a freshly written copy sorts behind an old unverified one and a heavy write rate cannot starve the sweep. Every attempt is stamped, including reads that fail, so an unreadable copy cannot stall the queue behind it. <code>GetLeastRecentlyScrubbedObjects</code> skips copies verified within <code>integrity.scrubber_min_age</code> (default 24 hours), so they are not re-read. Copies on backends over their usage limit are deferred to a later cycle and counted separately.</p><p><b>Interval</b>: configurable via <code>integrity.scrubber_interval</code> (default 6 hours, 0 = disabled).<br><b>Advisory lock</b>: <code>LockScrubber = 1010</code>.<br><b>Batch size</b>: configurable via <code>integrity.scrubber_batch_size</code> (default 100).<br><b>Guard</b>: only runs when <code>integrity.enabled: true</code> and <code>scrubber_interval > 0</code>. It runs on a tick only, never at startup, so an interval longer than the process lifetime means it never runs at all.</p><p>The stored form is undone before hashing, in the reverse of the order it was applied: decrypt, then decompress. The hash is always against the bytes the client wrote. A copy that cannot be decoded at all is reported as unreadable rather than corrupt, so it is left alone instead of deleted. Each backend read is tracked against usage quota (API calls + egress).</p><p>On hash mismatch: the bytes are removed via <code>DeleteOrEnqueue()</code> with reason <code>integrity_scrub_failed</code>, and the <code>object_locations</code> row is dropped so the replicator sees the object as under-replicated and rebuilds it. Leaving the row behind would let the replicator keep counting a copy that no longer exists.</p><p class="ac-metric">Metrics: s3o_integrity_checks_total{operation="scrub"}, s3o_integrity_errors_total{operation="scrub"}, s3o_integrity_oldest_unverified_seconds, s3o_integrity_never_verified_copies, s3o_integrity_deferred_copies, s3o_integrity_usage_declined_total</p>'
    },
    RECONCILE: {
      title: 'Orphan Reconciler',
      badge: 'process', badgeText: 'configurable (default 24h)',
      body: '<p>The scheduled <code>Reconciler.Run()</code> calls <code>SyncBackend()</code> for each backend. It lists the backend a page at a time, classifies every key on the page with one <code>ListedPathStates()</code> query (untracked, recorded, or pending cleanup), and imports the untracked keys through <code>ImportObject()</code>. It never deletes ledger rows. Backends with a drain record are skipped, and a backend\'s scan stops when its list-request budget is exhausted.</p><p><b>Interval</b>: configurable (default 24 hours).<br><b>Advisory lock</b>: <code>LockReconcile = 1009</code>.<br><b>Guard</b>: the service is registered only when <code>reconcile.enabled</code> is set at startup.</p><p><b>On-demand</b>: <code>POST /admin/api/reconcile[?backend=name]</code> runs <code>Reconciler.Reconcile</code>, which calls <code>ReconcileBackend()</code> instead. That pass diffs the backend against <code>object_locations</code> with a bounded-memory sorted-merge: it walks both sides as ascending path streams in byte order (S3 paginated by <code>ListObjects</code>, DB paginated by <code>ListObjectsByBackendKeyAsc</code> on <code>storage_key</code>, since a backend lists paths rather than object keys) and merges them in lockstep, so memory is O(page_size) regardless of object count. It imports S3-only keys through <code>ImportObject()</code> and removes DB-only rows through <code>DeleteObjectLocation()</code>.</p><p>Every key is imported at its literal backend key, used as both the object key and the storage key. A path already recorded as some row\'s storage key is skipped, so the per-write paths the orchestrator wrote are not adopted a second time as objects named after their path. Keys outside every configured virtual bucket prefix are imported too &mdash; those are real bytes against the backend&#39;s quota, so leaving them off the ledger makes space accounting wrong. Such rows are flagged <code>managed = false</code>: quota sums them, but replication, rebalance, integrity and drain skip them. After reconciling, quota metrics are refreshed.</p><p>A key whose delete is still outstanding &mdash; waiting in <code>cleanup_queue</code> or dead-lettered to <code>cleanup_dlq</code> &mdash; is left alone rather than imported. Those bytes are on the backend only because the delete could not reach them, so adopting the key would resurrect the object: it would come back live, the replicator would spread it to reach the replication factor, and its <code>created_at</code> would restart so any lifecycle rule that had expired it would wait another full window. The page query finds these keys up front, and the import transaction checks again, so a cleanup finishing concurrently cannot slip between the check and the insert.</p><p>The suppression is scoped to the <code>(storage_key, backend)</code> pair, so a copy removed cleanly on another backend is still importable. Suppressed keys are logged and counted as <code>suppressed_pending_cleanup</code>; a run reporting many of them points at a cleanup queue that is not draining rather than at a reconcile problem.</p><p>Every scheduled pass also runs <code>ReconcileUsage()</code>, which rewrites each backend&#39;s striped byte total to <code>SUM(object_locations.size_bytes)</code>. Mutations charge the counter inside the transaction that writes the rows it summarizes, so it cannot drift from them: this pass is an audit, and a correction it applies means a mutation path is storing bytes without charging them. Runs regardless of import count, and after an import (which adopts rows the counter has never seen) and is also available on demand via <code>POST /admin/api/usage-reconcile</code>.</p><p>Audit events: <code>storage.ReconcileComplete</code>. The scheduled pass carries <code>imported</code>, <code>skipped</code> and <code>duration</code>; the on-demand pass carries <code>imported</code>, <code>removed</code>, <code>suppressed_pending_cleanup</code> and <code>backends_scanned</code>. <code>usage.reconcile</code> carries the count of backends corrected.</p><p class="ac-metric">Metric: s3o_quota_reconcile_corrections_total</p>'
    },
    CBWATCH: {
      title: 'CB Watchdog',
      badge: 'filter', badgeText: 'every 5 s',
      body: '<p><code>breaker.NewWatchdog()</code> calls <code>Registry.ProbeAll()</code> every tick, which probes every circuit breaker in the registry (database, per-backend, Redis). Each breaker decides what a probe means for it.</p><p><b>Interval</b>: 5 seconds, which bounds how late a due health check can run.<br><b>Advisory lock</b>: none (per-instance, no coordination needed).</p><p>An open backend breaker runs a health check against its backend once its backoff is due and closes when the check passes, so a backend recovers without waiting for client traffic to probe it. A breaker whose half-open probe has been in flight too long (e.g. the backend accepted the connection but never responded) is reset to open so a fresh probe can be dispatched.</p>'
    },
    PG: {
      title: 'PostgreSQL',
      badge: 'storage', badgeText: 'shared state',
      body: '<p>Central metadata store shared by all background services. Hosts object locations, quota stats, usage counters, multipart upload state, the cleanup queue, the notification outbox, and advisory locks. All services query it for work items and write back results; the Usage Flusher is the main writer of counter data.</p><p><b>Advisory locks</b> provide leader election: <code>pg_try_advisory_lock(lockID)</code> ensures only one instance runs each service. Lock IDs: Rebalancer=1001, Replicator=1002, CleanupQueue=1003, MultipartCleanup=1004, Lifecycle=1005, Drain=1006, UsageFlush=1007, OverReplication=1008, Reconcile=1009, Scrubber=1010, PendingReaper=1011, Migrations=1012, FleetSnapshot=1013. The notification drainer currently takes the same lock 1011 as the pending reaper. <code>CompleteMultipartUpload</code> also takes a per-upload advisory lock derived from the upload ID.</p><p>Key tables: <code>object_locations</code>, <code>backend_quotas</code>, <code>backend_quota_stripes</code>, <code>backend_usage</code>, <code>backend_request_usage</code>, <code>backend_drains</code>, <code>cleanup_queue</code>, <code>cleanup_dlq</code>, <code>pending_objects</code>, <code>notification_outbox</code>, <code>multipart_uploads</code>, <code>multipart_parts</code>.</p>'
    },
    NOTIFY: {
      title: 'Notification Drainer',
      badge: 'process', badgeText: 'every 2 s',
      body: '<p><code>notify.Notifier</code> delivers webhook notifications from the <code>notification_outbox</code> table. Each tick it reads up to 50 pending rows and POSTs each one to its endpoint.</p><p><b>Interval</b>: 2 seconds.<br><b>Advisory lock</b>: 1011, the same ID as <code>LockPendingReaper</code>, so the drainer and the pending reaper currently share one lock and each skips a tick while the other holds it.<br><b>Guard</b>: registered only in worker or all mode, and only when notification endpoints are configured.</p><p>A failed delivery is retried with backoff of 2^attempts seconds, capped at 64 seconds. After the endpoint\'s <code>max_retries</code> (default 3) the row is removed.</p><p class="ac-metric">Metrics: s3o_notification_sent_total{endpoint,event_type}, s3o_notification_failed_total{endpoint,event_type}, s3o_notification_dropped_total, s3o_notification_queue_depth, s3o_notification_store_errors_total{operation}, s3o_notification_duration_seconds{endpoint}</p>'
    },
    PROVWATCH: {
      title: 'Provisioning Watch',
      badge: 'filter', badgeText: 'event-driven',
      body: '<p>Rebuilds this instance\'s provisioning view (buckets, users, credentials, grants) when another instance announces a provisioning change over Redis. The rebuild reads the provisioning tables and merges them with the config file.</p><p><b>Interval</b>: none; it reacts to messages on the shared Redis channel.<br><b>Advisory lock</b>: none (every instance keeps its own view).<br><b>Guard</b>: registered only when Redis is configured. Runs in every mode, since API instances authenticate against the view and workers read its declared buckets.</p>'
    },
    FLIGHTREC: {
      title: 'Flight Recorder',
      badge: 'filter', badgeText: 'optional',
      body: '<p><code>debug.FlightRecorderService</code> runs the Go runtime trace flight recorder in a bounded ring buffer. The admin trace-snapshot endpoint streams its most recent window on demand.</p><p><b>Interval</b>: none; it runs until shutdown.<br><b>Advisory lock</b>: none (per-instance).<br><b>Guard</b>: registered only when <code>debug.flight_recorder.enabled</code> is true. Runs in every mode.</p>'
    },
    S3: {
      title: 'S3 Backends',
      badge: 'storage', badgeText: 'object storage',
      body: '<p>Physical storage backends (OCI Object Storage, Cloudflare R2, etc.) accessed through the <code>ObjectBackend</code> interface, optionally wrapped with <code>CircuitBreakerBackend</code>.</p><p>Background services interact via:<br>&bull; <code>StreamCopy()</code>: piped <code>GetObject</code> &rarr; <code>PutObject</code> for rebalancer and replicator<br>&bull; <code>Delete()</code> / <code>DeleteMany()</code>: bounded single and batched deletes, each charged as the API calls actually sent; <code>DeleteMany</code> uses one <code>DeleteObjects</code> request per 1000 keys where the backend supports it<br>&bull; <code>DeleteObject()</code> / <code>DeleteOrEnqueue()</code>: lifecycle expiration deletes through the standard <code>DeleteObject</code> path, and over-replication through <code>DeleteOrEnqueue</code><br>&bull; <code>AbortMultipartUpload()</code>: deletes uploaded parts for stale upload cleanup</p><p>All S3 API calls are recorded against per-backend usage counters (<code>usage.Record()</code>) for quota enforcement. Each call names the operation it made, so it charges the request pools that operation belongs to as well as the backend&#39;s request total; operations listed as <code>unmetered</code> are recorded but charged to no pool.</p>'
    },
    CQ: {
      title: 'Cleanup Queue Table',
      badge: 'cleanup', badgeText: 'retry queue',
      body: '<p>PostgreSQL table <code>cleanup_queue</code> storing failed deletion operations for background retry.</p><p><b>Schema</b>: <code>id</code>, <code>backend_name</code>, <code>object_key</code>, <code>storage_key</code>, <code>reason</code>, <code>size_bytes</code>, <code>attempts</code>, <code>last_error</code>, <code>next_retry</code>, <code>created_at</code>, <code>claimed_at</code>, <code>claimed_by</code>.</p><p><b>Enqueue reasons</b>: <code>overwrite_displaced</code>, <code>delete_failed</code>, <code>batch_delete_failed</code>, <code>replication_orphan</code>, <code>over_replication</code>, <code>rebalance_orphan</code>, <code>rebalance_stale_orphan</code>, <code>rebalance_source_delete</code>, <code>drain_orphan</code>, <code>drain_stale_orphan</code>, <code>drain_source_delete</code>, <code>drain_race_aborted</code>, <code>abort_part_cleanup</code>, <code>complete_part_cleanup</code>, <code>orphan_record_failed</code>, <code>orphan_part_record_failed</code>, <code>integrity_scrub_failed</code>, <code>integrity_failed</code>, <code>superseded_intent</code>, <code>companion_discarded</code>, <code>companion_untrusted</code>.</p><p>Items are enqueued through <code>Coordinator.EnqueueCleanup()</code>, which also calls <code>IncrementOrphanBytes()</code> on the backend quota to prevent over-allocation. On a successful retry, <code>CompleteCleanupItem</code> atomically deletes the row and decrements <code>orphan_bytes</code> for the backing backend in a single CTE.</p><p>Worker claim filter: <code>WHERE next_retry <= NOW() AND attempts < 10 AND (claimed_at IS NULL OR claimed_at < graceCutoff)</code> with <code>FOR UPDATE SKIP LOCKED</code>; the matching <code>idx_cleanup_queue_claim (next_retry, created_at) WHERE attempts < 10</code> serves the <code>ORDER BY next_retry, created_at</code> scan without a sort. On the tenth consecutive failure the row is graduated to <code>cleanup_dlq</code> via <code>MoveCleanupToDLQ</code>; <code>orphan_bytes</code> is intentionally untouched because the bytes are still on disk and reclaim happens only when an operator confirms the object is gone.</p>'
    }
  };

  var tooltip = document.getElementById('ac-tooltip');
  var mouseX = 0, mouseY = 0;
  document.addEventListener('mousemove', function(e) {
    mouseX = e.clientX; mouseY = e.clientY;
    if (tooltip.style.display === 'block') positionTooltip();
  });
  function positionTooltip() {
    var pad = 12;
    var w = tooltip.offsetWidth, h = tooltip.offsetHeight;
    var vw = window.innerWidth, vh = window.innerHeight;

    var x = mouseX + pad;
    if (x + w > vw - pad) x = mouseX - w - pad;
    x = Math.max(pad, Math.min(x, vw - w - pad));

    // Prefer below the cursor, and flip above only when above genuinely has
    // more room. Clamping afterwards is what keeps a tall panel on screen: an
    // unclamped flip puts its top edge above the viewport, and a panel taller
    // than the viewport pins to the top and scrolls instead.
    var below = vh - mouseY - pad * 2;
    var above = mouseY - pad * 2;
    var y = (h <= below || below >= above) ? mouseY + pad : mouseY - h - pad;
    y = Math.max(pad, Math.min(y, vh - h - pad));

    tooltip.style.left = x + 'px';
    tooltip.style.top = y + 'px';
  }
  function showInfo(id) {
    var info = nodeInfo[id];
    if (!info) { tooltip.style.display = 'none'; return; }
    tooltip.innerHTML = '<h3>' + info.title + '</h3><span class="ac-badge ac-badge-' + info.badge + '">' + info.badgeText + '</span>' + info.body;
    tooltip.style.display = 'block'; positionTooltip();
  }
  function clearInfo() { tooltip.style.display = 'none'; }

  function wireUpInteractivity() {
    var svg = document.querySelector('#ac-diagram svg');
    if (!svg) return;
    var adj = {}, edgeMap = {};
    svg.querySelectorAll('.edgePath').forEach(function(ep, i) {
      var cls = ep.getAttribute('class') || '';
      var m = cls.match(/LS-(\S+)/), m2 = cls.match(/LE-(\S+)/);
      if (!m || !m2) return;
      edgeMap[i] = { from: m[1], to: m2[1], path: ep, label: svg.querySelectorAll('.edgeLabel')[i] };
      (adj[m[1]] = adj[m[1]] || []).push(i);
    });
    function bfs(startId, adjacency, getNext) {
      var visited = new Set([startId]), edges = new Set(), queue = [startId];
      while (queue.length) { var cur = queue.shift(); (adjacency[cur] || []).forEach(function(ei) {
        edges.add(ei); var next = getNext(edgeMap[ei]);
        if (!visited.has(next)) { visited.add(next); queue.push(next); }
      }); } return { nodes: visited, edges: edges };
    }
    var radj = {};
    Object.keys(edgeMap).forEach(function(i) { var e = edgeMap[i]; (radj[e.to] = radj[e.to] || []).push(Number(i)); });
    svg.querySelectorAll('.node').forEach(function(node) {
      var id = node.id.replace(/^flowchart-/, '').replace(/-\d+$/, '');
      node.addEventListener('mouseenter', function() {
        svg.classList.add('highlighting');
        var fwd = bfs(id, adj, function(e) { return e.to; });
        var bwd = bfs(id, radj, function(e) { return e.from; });
        var allNodes = new Set([...fwd.nodes, ...bwd.nodes]);
        var allEdges = new Set([...fwd.edges, ...bwd.edges]);
        svg.querySelectorAll('.node').forEach(function(n) {
          n.classList.toggle('highlight', allNodes.has(n.id.replace(/^flowchart-/, '').replace(/-\d+$/, '')));
        });
        Object.keys(edgeMap).forEach(function(i) {
          var hl = allEdges.has(Number(i));
          edgeMap[i].path.classList.toggle('highlight', hl);
          if (edgeMap[i].label) edgeMap[i].label.classList.toggle('highlight', hl);
        });
        showInfo(id);
      });
      node.addEventListener('mouseleave', function() {
        svg.classList.remove('highlighting');
        svg.querySelectorAll('.highlight').forEach(function(el) { el.classList.remove('highlight'); });
        clearInfo();
      });
    });
  }
})();
</script>

## Legend

| Color | Meaning |
|-------|---------|
| <span style="color:#1a7a5a">**Forest green**</span> | Scheduler / entry point |
| <span style="color:#c4a35a">**Amber**</span> | Service that runs on every instance |
| <span style="color:#5ec9a0">**Teal**</span> | Worker that needs worker or all mode |
| <span style="color:#4aaa8a">**Teal**</span> | Shared storage (PostgreSQL / S3) |
| <span style="color:#8a9aa8">**Gray**</span> | Cleanup / retry queue |

## Run Modes

In API-only mode only usage-flush, fleet-snapshot, cb-watchdog, flight-recorder and provisioning-watch run. The other services need worker or all mode. Flight-recorder runs only when it is enabled, and provisioning-watch only when Redis is configured.

## Service Summary

| Service | Interval | Advisory Lock ID | Key Function |
|---------|----------|------------------|--------------|
| Replicator | 5 min (configurable) | 1002 | `Replicator.Replicate()` |
| Rebalancer | 6 hrs (configurable) | 1001 | `Rebalancer.Rebalance()` |
| Over-Replication Cleaner | 5 min (configurable) | 1008 | `OverReplicationCleaner.Clean()` |
| Lifecycle Expiration | 1 hr | 1005 | `expiry.Manager.ProcessRules()` |
| Multipart Cleanup | 1 hr | 1004 | `CleanupStaleMultipartUploads()` |
| Usage Flusher | 30s (adaptive) | 1007 (Redis only) | `FlushUsage()` |
| Fleet Snapshot | 60s (`fleet_interval`) | 1013 | `Collector.RefreshFleetIfStale()` |
| Cleanup Queue Worker | 1 min | 1003 | `ProcessCleanupQueue()` |
| Pending Reaper | 1 min (configurable) | 1011 | `PendingReaper.ProcessPendingQueue()` |
| Backend Drain | 10s | 1006 | `Drainer.Drain()` |
| Orphan Reconciler | 24 hrs (configurable) | 1009 | `Reconciler.Run()` |
| Integrity Scrubber | 6 hrs (configurable) | 1010 | `Scrubber.Scrub()` |
| Notification Drainer | 2s | 1011 (shared with the pending reaper) | `notify.Notifier.Run()` |
| CB Watchdog | 5s | none (per-instance) | `breaker.NewWatchdog()` / `Registry.ProbeAll()` |
| Provisioning Watch | on Redis message | none (per-instance) | `newProvisioningWatcher()` |
| Flight Recorder | continuous | none (per-instance) | `debug.FlightRecorderService.Run()` |

