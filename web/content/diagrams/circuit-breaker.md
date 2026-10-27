---
description: "Interactive state machine for the circuit breaker that guards the database, each backend and each of its calls, and the Redis counter backend."
title: "Circuit Breaker"
linkTitle: "Circuit Breaker"
weight: 4
---

State machine of `breaker.CircuitBreaker`, the one breaker type used in three places: the database breaker wraps every SQL call in the store driver (SQLite or PostgreSQL), backend breakers (`CircuitBreakerBackend`, enabled with `backend_circuit_breaker.enabled`) wrap every backend call, and the Redis counter breaker counts Redis failures. Every breaker has two states. While open, no call reaches the database, the backend or Redis, and only a separate health check closes the breaker, going straight from open to closed. Each backend runs this state machine in two layers: a backend breaker for failures that say the whole backend is broken, and one breaker per call (`PutObject`, `GetObject`, `HeadObject`, `DeleteObject`, `DeleteObjects`, `CopyObject`, `ListObjects`) for the backend refusing that call. A call reaches the backend only while both its own breaker and the backend breaker are closed.

The calls a backend is serving form one bitset, `core.BackendCalls`, read with a single load. Each consumer tests it against the mask for its purpose: `RequiredForWrite` (`PutObject` and `GetObject`) for client writes, replicas, drains and rebalance moves through `WriteTargets`; `RequiredForRead` (`GetObject`) for replication sources and over-replication scoring; and `RequiredForProbe` (`HeadObject`) for the pending reaper. The same set gives the health that `admin status`, the TUI and the dashboard show: healthy when every call is served, down when none is, degraded in between. **Hover over any component** for implementation details.

{{< diagram >}}

<script>
window.s3oDiagram = {
  source: [
    'flowchart TD',
    '    CALL([cb.Call]):::entry --> PRE{PreCheck}:::decision',
    '',
    '    PRE -->|state = closed| EXEC[Execute<br>Operation]:::process',
    '    PRE -->|state = open| SENTINEL[Return Sentinel<br>Error]:::reject',
    '',
    '    EXEC --> POST{PostCheck<br>Error Filter}:::decision',
    '    POST -->|not a circuit error| RESET[Reset Failure<br>Counter]:::process',
    '    RESET --> SUCCESS[Return<br>Result]:::success',
    '',
    '    POST -->|circuit error| FAIL[Increment<br>Failures]:::filter',
    '    FAIL --> THRESH{Failures at<br>Threshold?}:::decision',
    '    THRESH -->|no| PASSTHRU[Return<br>Original Error]:::process',
    '    THRESH -->|yes| TRIP[Transition<br>to Open]:::reject',
    '',
    '    WATCHDOG([Health Check<br>Loop]):::entry --> HEALTH{Health Check<br>Passes?}:::decision',
    '    HEALTH -->|no| BACKOFF[Stay Open,<br>Check Again Later]:::reject',
    '    HEALTH -->|yes| EXTRECOVER[Recover:<br>Open to Closed]:::success',
    '',
  ].join('\n'),

  info: {
    CALL: {
      title: 'cb.Call',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Methods on <code>breaker.CircuitBreaker</code> that guard one operation. <code>cb.Call(fn)</code> runs a function returning <code>(T, error)</code>; a caller with an <code>error</code>-only operation runs PreCheck and PostCheck around it directly.</p><p><code>CircuitBreakerBackend</code> guards PutObject, GetObject, HeadObject, DeleteObject, DeleteObjects, CopyObject and ListObjects this way, running each call through the backend breaker and then that call\'s own breaker. The database breaker runs the same PreCheck and PostCheck around every SQL Exec, Query and QueryRow in the store driver (SQLite or PostgreSQL).</p><p>The Redis counter backend never calls PreCheck. It only feeds call results to PostCheck to count failures, and switches to local counters once the breaker opens.</p><p>Flow: PreCheck &rarr; execute &rarr; PostCheck. If PreCheck returns an error, the real operation is never called.</p>'
    },
    PRE: {
      title: 'PreCheck',
      badge: 'decision', badgeText: 'state machine gate',
      body: '<p><code>cb.PreCheck()</code> is the entry gate that reads the current circuit state.</p><p><b>Closed</b>: returns <code>nil</code> &mdash; all calls pass through to the real operation.<br><b>Open</b>: returns the sentinel error, however long the circuit has been open. No call is let through to find out whether the dependency is back; the health check loop does that.</p><p>This is the fast-rejection path: open circuits never touch the real backend or database.</p>'
    },
    EXEC: {
      title: 'Execute Operation',
      badge: 'process', badgeText: 'real call',
      body: '<p>Calls the wrapped function &mdash; the actual S3 backend call or database query. This only runs when PreCheck returns <code>nil</code>, which means the circuit is closed.</p><p>For backends: <code>cb.inner.PutObject()</code>, <code>cb.inner.GetObject()</code>, etc.<br>For database: <code>inner.Exec()</code>, <code>inner.Query()</code>, <code>inner.QueryRow()</code> on the wrapped sqlc DBTX (PostgreSQL), or the matching <code>database/sql</code> calls (SQLite).</p><p>The result and error are passed to PostCheck for state machine evaluation.</p>'
    },
    SENTINEL: {
      title: 'Return Sentinel Error',
      badge: 'reject', badgeText: 'fast rejection',
      body: '<p>Returns the circuit-specific sentinel error immediately, with no I/O to the underlying system.</p><p><b>Backend</b>: <code>ErrBackendUnavailable</code> &mdash; a write fails over to the next eligible backend, and a read fails over to the next replica.<br><b>Database</b>: <code>ErrDBUnavailable</code> &mdash; triggers degraded mode: writes return 503, and reads fan out to all backends while <code>circuit_breaker.degraded_reads_enabled</code> is on (the default).<br><b>Redis</b>: an internal <code>redis unavailable</code> error &mdash; the counter backend switches to local in-memory counters.</p><p>Writes, replicas, drains and rebalance moves pick only from <code>WriteTargets</code>, which drops backends not serving both <code>PutObject</code> and <code>GetObject</code>, so they rarely meet this error. Reads and deletes still try a refusing backend, get this error, and fail over or leave the delete queued for retry.</p>'
    },
    POST: {
      title: 'PostCheck / Error Filter',
      badge: 'decision', badgeText: 'result evaluation',
      body: '<p><code>cb.PostCheck(err)</code> passes the operation result through the pluggable error filter (<code>cb.isError(err)</code>) to decide whether it counts as a circuit-breaker failure. Each filter counts only failures that say the dependency cannot serve; an error about one request never counts.</p><p><b>Backend filters</b> (<code>classifyFailure</code>): an error with no HTTP status (connection refused, DNS, TLS, reset) or a 401 counts against the backend breaker. A 5xx, 429 or 403 counts against the breaker of the call that got it, so a provider refusing one call - downloads over a quota - opens only that call. Other statuses, such as a missing key or a failed precondition, count against neither. Context cancellation and deadline errors do not count either, nor does a provider declining batch delete or a refusal from one of the backend\'s own breakers.</p><p><b>Database filter</b> (<code>store.NewDatabaseBreaker</code>): a broken connection counts. So do the PostgreSQL SQLSTATEs that mean the server cannot serve (connection exception <code>08</code>, invalid authorization <code>28</code>, insufficient resources <code>53</code>, shutdown <code>57P</code>, system error <code>58</code>, internal error <code>XX</code>, missing database <code>3D000</code>) and the SQLite result codes that mean the file cannot be used (<code>CANTOPEN</code>, <code>CORRUPT</code>, <code>FULL</code>, <code>IOERR</code>, <code>NOMEM</code>, <code>NOTADB</code>, <code>PERM</code>, <code>READONLY</code>). Any other error the server answered with does not count: a duplicate key, a serialization conflict, a constraint violation, a cancelled statement or a busy database is about one request. Neither do a cancelled or expired context, the no-rows sentinels, a row that could not be scanned, or the application signals <code>S3Error</code> and <code>ErrNoSpaceAvailable</code>.</p><p>If the error counts, flows to failure handling. If not (or nil), flows to success.</p>'
    },
    RESET: {
      title: 'Reset Failure Counter',
      badge: 'process', badgeText: 'counter reset',
      body: '<p><code>cb.failures = 0</code> &mdash; clears the consecutive failure counter on any outcome this breaker does not count: a success, or an error its filter passes over. This keeps intermittent errors in otherwise healthy traffic from tripping the breaker.</p><p>It never closes an open circuit. A call that succeeds while the circuit is open went around PreCheck, as the Redis hot path does, and only <code>Recover</code> closes the circuit.</p>'
    },
    SUCCESS: {
      title: 'Return Result',
      badge: 'success', badgeText: 'success',
      body: '<p>The operation succeeded, or its error was not a circuit-breaker failure. Returns the result to the caller. The circuit remains closed and all subsequent requests continue passing through normally.</p>'
    },
    FAIL: {
      title: 'Increment Failures',
      badge: 'filter', badgeText: 'failure tracking',
      body: '<p><code>cb.onFailure()</code> increments <code>cb.failures</code> and records the error for the opening log line.</p><p>The failure counter tracks <b>consecutive</b> failures. A backend\'s backend breaker and its call breaker both see every guarded call, so a 403 resets the backend breaker\'s count and a connection error resets the call breaker\'s.</p>'
    },
    THRESH: {
      title: 'Failures >= Threshold?',
      badge: 'decision', badgeText: 'threshold check',
      body: '<p>Compares <code>cb.failures >= cb.failThreshold</code>.</p><p>Configurable thresholds:<br><b>Database</b>: <code>circuit_breaker.failure_threshold</code> (default: 3)<br><b>Backend</b>: <code>backend_circuit_breaker.failure_threshold</code> (default: 5)<br><b>Redis</b>: <code>redis.failure_threshold</code> (default: 3)</p><p>Backend values apply only when <code>backend_circuit_breaker.enabled: true</code> (default: false).</p><p>Below threshold, the error is returned to the caller as-is (not replaced with the sentinel), allowing the caller to handle it normally while the breaker continues tracking.</p>'
    },
    PASSTHRU: {
      title: 'Return Original Error',
      badge: 'process', badgeText: 'below threshold',
      body: '<p>Failure count is below the threshold. The original error is returned to the caller unchanged. The circuit remains closed.</p><p>The caller handles the error normally &mdash; for backends, this may trigger write failover to another backend. For database operations, the error propagates to the HTTP handler.</p><p>The failure counter persists: if the next call also fails, it increments further toward the threshold. A single outcome this breaker does not count resets the counter to 0.</p>'
    },
    TRIP: {
      title: 'Transition to Open (threshold reached)',
      badge: 'reject', badgeText: 'state transition',
      body: '<p><code>cb.transition(StateOpen)</code> &mdash; consecutive failure threshold reached. The circuit opens, and <code>cb.openedAt</code> is recorded for duration tracking.</p><p>Logs: <code>"opened: failure threshold reached"</code> with <code>component=circuit_breaker</code>, <code>breaker_name</code>, failure count, threshold and the last error.</p><p>PostCheck wraps the sentinel around the original error with <code>fmt.Errorf("%w: %w", sentinel, err)</code>. Callers match the sentinel with <code>errors.Is</code>, and logs still show what actually failed.</p><p>A backend\'s breakers are named <code>&lt;backend&gt;</code> for the backend breaker and <code>&lt;backend&gt;/&lt;Call&gt;</code> for a call breaker, such as <code>c2/GetObject</code>. Each publishes <code>backend.circuit.opened</code> with <code>call</code> set to the call name, or <code>all</code> for the backend breaker. The database and Redis breakers publish the same event without <code>call</code>.</p><p class="ac-metric">Database and Redis: s3o_circuit_breaker_transitions_total{name, from="closed", to="open"}, s3o_circuit_breaker_state{name} = 1<br>Backend: s3o_backend_call_refused{backend, call} = 1 for each refused call. s3o_circuit_breaker_state{name=&lt;backend&gt;} goes to 1, with one transition, when the backend stops serving every call; a second breaker opening does not move it.</p>'
    },
    WATCHDOG: {
      title: 'Health Check Loop',
      badge: 'entry', badgeText: 'out-of-band recovery',
      body: '<p>No breaker lets a real call through as a probe. A separate loop checks the dependency while the breaker is open.</p><p><b>Database and backends</b>: the breaker watchdog ticks every 5s and calls <code>breaker.RecoveryProber.Probe</code> for the database breaker and for each of every backend\'s breakers, each prober given its breaker\'s own check. The first check runs <code>max(open_timeout, 5s)</code> after the first tick that sees the circuit open. The open timeouts are <code>circuit_breaker.open_timeout</code> (default: 15s) for the database and <code>backend_circuit_breaker.open_timeout</code> (default: 5m) for backends.</p><p><b>Redis</b>: a background goroutine PINGs Redis every 5s while the counter backend is in fallback; <code>redis.open_timeout</code> has no effect on recovery.</p>'
    },
    HEALTH: {
      title: 'Health Check Passes?',
      badge: 'decision', badgeText: 'out-of-band check',
      body: '<p><b>Database</b>: a ping on the connection pool (PostgreSQL) or the database handle (SQLite), outside the breaker. A ping costs nothing, so it is not charged anywhere.</p><p><b>Backend breaker</b>: a <code>HeadBucket</code> call on the wrapped backend, bypassing the breakers. Every call comes back when it passes.<br><b>Call breakers</b>: a repeat of the call itself. GET and HEAD retry the last key the backend refused, or <code>.s3o-probe</code> before any was refused; GET asks for one byte, and a 404 counts as recovered. Listing, the deletes, PUT and copy use the reserved key <code>.s3o-probe</code>: they list it, delete it, and write it as an empty object, the copy check with a <code>PutObject</code>. A call check passes on an answer neither breaker counts, such as a 404 or 412; a refusal, an unreachable backend, a 401 or a timeout keeps it open. Each backend check is admitted and charged against the backend\'s usage budget as the call its breaker guards (<code>HeadBucket</code> for the backend breaker); a check the budget would refuse is skipped until there is room.</p><p>Each check has a 10s timeout.</p><p><b>Redis</b>: a <code>PING</code>. The charges kept locally during the outage need nothing on recovery: the usage flush writes them to the store.</p>'
    },
    BACKOFF: {
      title: 'Stay Open, Check Again Later',
      badge: 'reject', badgeText: 'still down',
      body: '<p>The circuit stays open and every call keeps getting the sentinel error.</p><p><b>Database</b>: the next ping runs one open timeout later, with no backoff.<br><b>Backend</b>: the wait before the next check doubles after each failure, capped at <code>max(5m, open_timeout)</code>.</p><p>Logs: <code>"recovery check failed; circuit stays open"</code> with <code>breaker_name</code> and <code>next_check_in</code>.</p><p><b>Redis</b>: the next PING runs on the next 5s tick.</p>'
    },
    EXTRECOVER: {
      title: 'Recover: Open to Closed',
      badge: 'success', badgeText: 'state transition',
      body: '<p><code>cb.Recover()</code> moves the breaker from open to closed and resets the failure counter, so the breaker again tolerates the configured threshold of failures.</p><p>Logs: <code>"closed: recovered"</code> with <code>component=circuit_breaker</code>, <code>breaker_name</code>, and <code>degraded_duration</code>. A backend breaker publishes <code>backend.circuit.closed</code> carrying its <code>call</code>; the database breaker also leaves degraded mode.</p><p class="ac-metric">Database and Redis: s3o_circuit_breaker_transitions_total{name, from="open", to="closed"}, s3o_circuit_breaker_state{name} = 0<br>Backend: s3o_backend_call_refused{backend, call} = 0 for each call served again. s3o_circuit_breaker_state{name=&lt;backend&gt;} returns to 0 only once every call is served.</p>'
    }
  }
};
</script>

## Legend

| Node | Meaning |
|------|---------|
| <span class="dg-swatch dg-entry"></span> | Entry point |
| <span class="dg-swatch dg-filter"></span> | Failure tracking |
| <span class="dg-swatch dg-decision"></span> | Decision / branch |
| <span class="dg-swatch dg-process"></span> | Processing step |
| <span class="dg-swatch dg-success"></span> | Success / recovery |
| <span class="dg-swatch dg-reject"></span> | Rejection / circuit open |
