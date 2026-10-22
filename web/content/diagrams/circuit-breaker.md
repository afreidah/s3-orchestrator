---
description: "Interactive state machine for the circuit breaker that guards the database, each backend and each of its calls, and the Redis counter backend."
title: "Circuit Breaker"
linkTitle: "Circuit Breaker"
weight: 4
---

State machine of `breaker.CircuitBreaker`, the one breaker type used in three places. The database breaker wraps every SQL call in the store driver (SQLite or PostgreSQL) and recovers through half-open probing: after the open timeout, one real query is let through as a probe. Backend breakers (`CircuitBreakerBackend`, enabled with `backend_circuit_breaker.enabled`) and the Redis counter breaker use external recovery instead: while open no call reaches the backend or Redis, and only a separate check closes them, going straight from open to closed. Each backend runs this state machine in two layers: a backend breaker for failures that say the whole backend is broken, and one breaker per call (`PutObject`, `GetObject`, `HeadObject`, `DeleteObject`, `DeleteObjects`, `CopyObject`, `ListObjects`) for the backend refusing that call. A call reaches the backend only while both its own breaker and the backend breaker are closed.

The calls a backend is serving form one bitset, `core.BackendCalls`, read with a single load. Each consumer tests it against the mask for its purpose: `RequiredForWrite` (`PutObject` and `GetObject`) for client writes, replicas, drains and rebalance moves through `WriteTargets`; `RequiredForRead` (`GetObject`) for replication sources and over-replication scoring; and `RequiredForProbe` (`HeadObject`) for the pending reaper. The same set gives the health that `admin status`, the TUI and the dashboard show: healthy when every call is served, down when none is, degraded in between. **Hover over any component** for implementation details.

{{< diagram >}}

<script>
window.s3oDiagram = {
  source: [
    'flowchart TD',
    '    CALL([cb.Call /<br>cb.CallNoResult]):::entry --> PRE{PreCheck}:::decision',
    '',
    '    PRE -->|state = closed| EXEC[Execute<br>Operation]:::process',
    '',
    '    PRE -->|open, backend breaker| SENTINEL[Return Sentinel<br>Error]:::reject',
    '    PRE -->|open, database breaker| TIMEOUT{Open Timeout<br>Elapsed?<br>database only}:::decision',
    '    TIMEOUT -->|no| SENTINEL',
    '    TIMEOUT -->|yes| PROBE{Probe Slot<br>Available?<br>database only}:::decision',
    '    PROBE -->|CAS fails| SENTINEL',
    '    PROBE -->|CAS ok| HALFOPEN[Transition<br>to Half-Open<br>database only]:::process',
    '    HALFOPEN --> EXEC',
    '',
    '    PRE -->|state = half-open| STALE{Probe Stale?<br>over 2 min<br>database only}:::decision',
    '    STALE -->|no| SENTINEL',
    '    STALE -->|yes / watchdog| REOPEN2[Reset to<br>Open]:::reject',
    '',
    '    EXEC --> POST{PostCheck<br>Error Filter}:::decision',
    '    POST -->|not a circuit error| SUCCESS[Return<br>Result]:::success',
    '',
    '    POST -->|circuit error| FAIL[Increment<br>Failures]:::filter',
    '    FAIL --> WASHO{Was<br>Half-Open?<br>database only}:::decision',
    '    WASHO -->|yes| REOPEN[Transition<br>to Open<br>database only]:::reject',
    '    WASHO -->|no| THRESH{Failures at<br>Threshold?}:::decision',
    '    THRESH -->|no| PASSTHRU[Return<br>Original Error]:::process',
    '    THRESH -->|yes| TRIP[Transition<br>to Open]:::reject',
    '',
    '    POST -->|success + half-open| RECOVER[Transition<br>to Closed<br>database only]:::success',
    '    RECOVER --> RESET[Reset Failure<br>Counter]:::process',
    '    RESET --> SUCCESS',
    '',
    '    WATCHDOG([Backend and Redis<br>Health Check Loop]):::entry --> HEALTH{Health Check<br>Passes?}:::decision',
    '    HEALTH -->|no| BACKOFF[Stay Open,<br>Check Again Later]:::reject',
    '    HEALTH -->|yes| EXTRECOVER[Recover:<br>Open to Closed]:::success',
    '',
  ].join('\n'),

  info: {
    CALL: {
      title: 'cb.Call / cb.CallNoResult',
      badge: 'entry', badgeText: 'entry point',
      body: '<p>Methods on <code>breaker.CircuitBreaker</code> that guard one operation. <code>cb.Call(fn)</code> handles functions returning <code>(T, error)</code>; <code>cb.CallNoResult(fn)</code> handles <code>error</code>-only functions.</p><p><code>CircuitBreakerBackend</code> guards PutObject, GetObject, HeadObject, DeleteObject, DeleteObjects, CopyObject and ListObjects this way, running each call through the backend breaker and then that call\'s own breaker. The database breaker runs the same PreCheck and PostCheck around every SQL Exec, Query and QueryRow in the store driver (SQLite or PostgreSQL).</p><p>The Redis counter backend never calls PreCheck. It only feeds call results to PostCheck to count failures.</p><p>Flow: PreCheck &rarr; execute &rarr; PostCheck. If PreCheck returns an error, the real operation is never called.</p>'
    },
    PRE: {
      title: 'PreCheck',
      badge: 'decision', badgeText: 'state machine gate',
      body: '<p><code>cb.PreCheck()</code> is the entry gate that inspects the current circuit state under a mutex lock.</p><p><b>Closed</b>: returns <code>nil</code> &mdash; all calls pass through to the real operation.<br><b>Open, backend breaker</b>: always returns the sentinel error. Backend breakers never go half-open.<br><b>Open, database breaker</b>: checks if <code>openTimeout</code> plus jitter has elapsed since <code>lastFailure</code>. If not, returns the sentinel error immediately (no I/O).<br><b>Half-Open</b> (database only): returns sentinel &mdash; only the single probe request is allowed through.</p><p>This is the fast-rejection path: open circuits never touch the real backend or database.</p>'
    },
    EXEC: {
      title: 'Execute Operation',
      badge: 'process', badgeText: 'real call',
      body: '<p>Calls the wrapped function &mdash; the actual S3 backend call or database query. This only runs when PreCheck returns <code>nil</code> (circuit closed, or the database probe allowed through).</p><p>For backends: <code>cb.inner.PutObject()</code>, <code>cb.inner.GetObject()</code>, etc.<br>For database: <code>inner.Exec()</code>, <code>inner.Query()</code>, <code>inner.QueryRow()</code> on the wrapped sqlc DBTX (PostgreSQL), or the matching <code>database/sql</code> calls (SQLite).</p><p>The result and error are passed to PostCheck for state machine evaluation.</p>'
    },
    TIMEOUT: {
      title: 'Open Timeout Elapsed? (database only)',
      badge: 'decision', badgeText: 'recovery timer',
      body: '<p>Checks <code>time.Since(cb.lastFailure) >= cb.openTimeout + cb.probeJitter</code>. This check runs only for the database breaker.</p><p><b>Database</b>: <code>circuit_breaker.open_timeout</code> (default: 15s).</p><p>The same setting means something else for the other breakers:<br><b>Backend</b>: <code>backend_circuit_breaker.open_timeout</code> (default: 5m, applies only when <code>backend_circuit_breaker.enabled: true</code>) is the delay before the first health check.<br><b>Redis</b>: <code>redis.open_timeout</code> (default: 15s) has no effect on recovery; Redis is PINGed every 5s while in fallback.</p><p>A random <code>probeJitter</code> of up to <code>openTimeout/4</code> is added on each open transition to prevent multiple circuit breakers from probing simultaneously after a shared failure event.</p><p>Until the timeout + jitter elapses, all requests receive the sentinel error without any I/O.</p>'
    },
    SENTINEL: {
      title: 'Return Sentinel Error',
      badge: 'reject', badgeText: 'fast rejection',
      body: '<p>Returns the circuit-specific sentinel error immediately, with no I/O to the underlying system.</p><p><b>Backend</b>: <code>ErrBackendUnavailable</code> &mdash; a write fails over to the next eligible backend, and a read fails over to the next replica.<br><b>Database</b>: <code>ErrDBUnavailable</code> &mdash; triggers degraded mode: writes return 503, and reads fan out to all backends while <code>circuit_breaker.degraded_reads_enabled</code> is on (the default).<br><b>Redis</b>: an internal <code>redis unavailable</code> error &mdash; the counter backend switches to local in-memory counters.</p><p>Writes, replicas, drains and rebalance moves pick only from <code>WriteTargets</code>, which drops backends not serving both <code>PutObject</code> and <code>GetObject</code>, so they rarely meet this error. Reads and deletes still try a refusing backend, get this error, and fail over or leave the delete queued for retry.</p>'
    },
    PROBE: {
      title: 'Probe Slot Available? (database only)',
      badge: 'decision', badgeText: 'atomic CAS',
      body: '<p>Database breaker only. <code>cb.probeInFlight.CompareAndSwap(false, true)</code> &mdash; atomic compare-and-swap ensures exactly <b>one</b> probe request passes through at a time.</p><p>If a probe is already in flight (another goroutine won the CAS), this request gets the sentinel error. This prevents a thundering herd of probe requests when multiple goroutines detect the timeout simultaneously.</p><p>The <code>probeInFlight</code> flag is cleared in both <code>onSuccess()</code> (probe succeeded, circuit closes) and <code>onFailure()</code> (probe failed, circuit re-opens).</p>'
    },
    HALFOPEN: {
      title: 'Transition to Half-Open (database only)',
      badge: 'process', badgeText: 'state transition',
      body: '<p><code>cb.transition(StateHalfOpen)</code> &mdash; allows exactly one real query through to test whether the database has recovered. Backend and Redis breakers never enter this state.</p><p>Logs: <code>"half-open: probing"</code> with <code>component=circuit_breaker</code>, <code>breaker_name</code>, and <code>open_duration</code> showing how long the circuit was open.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name, from="open", to="half-open"}<br>Gauge: s3o_circuit_breaker_state{name} = 2</p>'
    },
    POST: {
      title: 'PostCheck / Error Filter',
      badge: 'decision', badgeText: 'result evaluation',
      body: '<p><code>cb.PostCheck(err)</code> passes the operation result through the pluggable error filter (<code>cb.isError(err)</code>) to decide whether it counts as a circuit-breaker failure.</p><p><b>Backend filters</b> (<code>classifyFailure</code>): an error with no HTTP status (connection refused, DNS, TLS, reset) or a 401 counts against the backend breaker. A 5xx, 429 or 403 counts against the breaker of the call that got it, so a provider refusing one call - downloads over a quota - opens only that call. Other statuses, such as a missing key or a failed precondition, count against neither. Context cancellation and deadline errors do not count either, nor does a provider declining batch delete or a refusal from one of the backend\'s own breakers.</p><p><b>Database filter</b> (<code>isDBError</code>): exempts application-level errors (<code>S3Error</code>, <code>ErrNoSpaceAvailable</code>) and the no-rows sentinels (<code>sql.ErrNoRows</code>, <code>pgx.ErrNoRows</code>). Every other error trips the breaker.</p><p>If the error passes the filter, flows to failure handling. If not (or nil), flows to success.</p>'
    },
    SUCCESS: {
      title: 'Return Result',
      badge: 'success', badgeText: 'success',
      body: '<p>The operation succeeded (or the error was not a circuit-breaker error). Returns the result to the caller.</p><p>If the circuit was closed, the failure counter was reset to 0 by <code>onSuccess()</code>. The circuit remains closed and all subsequent requests continue passing through normally.</p>'
    },
    FAIL: {
      title: 'Increment Failures',
      badge: 'filter', badgeText: 'failure tracking',
      body: '<p><code>cb.onFailure()</code> increments <code>cb.failures++</code> and records <code>cb.lastFailure = time.Now()</code>.</p><p>The failure counter tracks <b>consecutive</b> failures. It is reset to 0 via <code>onSuccess()</code> on any call outcome this breaker does not count: a success, or an error its filter passes over. A backend\'s backend breaker and its call breaker both see every guarded call, so a 403 resets the backend breaker\'s count and a connection error resets the call breaker\'s. This means intermittent errors (occasional timeouts in otherwise healthy traffic) do not trip the breaker.</p><p>The <code>lastFailure</code> timestamp is used by the database breaker to calculate when the <code>openTimeout</code> elapses for probe eligibility.</p>'
    },
    WASHO: {
      title: 'Was Half-Open? (database only)',
      badge: 'decision', badgeText: 'probe result',
      body: '<p>Only the database breaker can be half-open. If it was in <code>StateHalfOpen</code> when the failure occurred, the probe query failed &mdash; the database is still down.</p><p>The <code>probeInFlight</code> atomic flag is cleared (<code>Store(false)</code>) so a future probe can be attempted after the open timeout elapses again.</p><p>The circuit transitions directly back to open without waiting for the failure threshold.</p>'
    },
    REOPEN: {
      title: 'Transition to Open (probe failed, database only)',
      badge: 'reject', badgeText: 'state transition',
      body: '<p><code>cb.transition(StateOpen)</code> &mdash; the database probe failed, circuit re-opens. The <code>lastFailure</code> timestamp resets the open timeout window, and a new jitter is drawn.</p><p>Logs: <code>"reopened: probe failed"</code> with <code>component=circuit_breaker</code>, <code>breaker_name</code>, and the current failure count.</p><p>The cycle repeats: after <code>openTimeout</code> plus jitter elapses, another single probe is attempted.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name, from="half-open", to="open"}<br>Gauge: s3o_circuit_breaker_state{name} = 1</p>'
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
    RECOVER: {
      title: 'Transition to Closed (database only)',
      badge: 'success', badgeText: 'state transition',
      body: '<p><code>cb.transition(StateClosed)</code> &mdash; the database probe succeeded, the database is healthy again.</p><p>Logs: <code>"closed: recovered"</code> with <code>component=circuit_breaker</code>, <code>breaker_name</code>, and <code>degraded_duration</code> showing total time spent in open + half-open states.</p><p>The <code>probeInFlight</code> flag is cleared and all subsequent requests pass through normally.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name="database", from="half-open", to="closed"}<br>Gauge: s3o_circuit_breaker_state{name} = 0</p>'
    },
    STALE: {
      title: 'Probe Stale? (database only)',
      badge: 'decision', badgeText: 'stale probe check',
      body: '<p>While the database breaker is half-open, every other call gets the sentinel error. PreCheck also checks how long the current probe has been in flight.</p><p>If it has been 2 minutes or more (<code>probeTimeout</code>), the probe is treated as abandoned and the breaker resets to open. Otherwise the call is refused and the probe keeps running.</p>'
    },
    REOPEN2: {
      title: 'Reset to Open (stale probe, database only)',
      badge: 'reject', badgeText: 'state transition',
      body: '<p>A half-open probe that has been in flight for 2 minutes or more is treated as abandoned, for example when PostCheck was never called. The breaker clears the probe flag and goes back to open, so a later request can probe again.</p><p>PreCheck does this when a request arrives. The breaker watchdog also does it on its 5s tick through <code>ResetStaleProbe()</code>, so a stale probe is cleared even with no traffic.</p><p class="ac-metric">Metric: s3o_circuit_breaker_transitions_total{name="database", from="half-open", to="open"}</p>'
    },
    WATCHDOG: {
      title: 'Backend and Redis Health Check Loop',
      badge: 'entry', badgeText: 'external recovery',
      body: '<p>Backend and Redis breakers never let a real call through as a probe. A separate loop checks the dependency while the breaker is open.</p><p><b>Backend</b>: the breaker watchdog ticks every 5s and calls <code>RecoveryProber.Probe</code> for each of every backend\'s breakers, each prober given that breaker\'s own check. The first check runs <code>max(open_timeout, 5s)</code> after the first tick that sees the circuit open.</p><p><b>Redis</b>: a background goroutine PINGs Redis every 5s while the counter backend is in fallback.</p>'
    },
    HEALTH: {
      title: 'Health Check Passes?',
      badge: 'decision', badgeText: 'out-of-band check',
      body: '<p><b>Backend breaker</b>: a <code>HeadBucket</code> call on the wrapped backend, bypassing the breakers. Every call comes back when it passes.<br><b>Call breakers</b>: a repeat of the call itself. GET and HEAD retry the last key the backend refused, or <code>.s3o-probe</code> before any was refused; GET asks for one byte, and a 404 counts as recovered. Listing, the deletes, PUT and copy use the reserved key <code>.s3o-probe</code>: they list it, delete it, and write it as an empty object, the copy check with a <code>PutObject</code>. A call check passes on an answer neither breaker counts, such as a 404 or 412; a refusal, an unreachable backend, a 401 or a timeout keeps it open.</p><p>Each check has a 10s timeout and is admitted and charged against the backend\'s usage budget as the call its breaker guards (<code>HeadBucket</code> for the backend breaker). A check the budget would refuse is skipped until there is room.</p><p><b>Redis</b>: a <code>PING</code>. On success, the local counter deltas collected during the outage are replayed to Redis in one pipeline before the breaker closes.</p>'
    },
    BACKOFF: {
      title: 'Stay Open, Check Again Later',
      badge: 'reject', badgeText: 'still down',
      body: '<p>The circuit stays open and every call keeps getting the sentinel error.</p><p><b>Backend</b>: the wait before the next check doubles after each failure, capped at <code>max(5m, open_timeout)</code>. Logs: <code>"recovery check failed; circuit stays open"</code> with <code>operation</code> and <code>next_check_in</code>.</p><p><b>Redis</b>: the next PING runs on the next 5s tick.</p>'
    },
    EXTRECOVER: {
      title: 'Recover: Open to Closed',
      badge: 'success', badgeText: 'state transition',
      body: '<p><code>cb.Recover()</code> moves the breaker straight from open to closed, with no half-open step. It clears probe state and resets the failure counter, so the breaker again tolerates the configured threshold of failures.</p><p>Logs: <code>"closed: recovered"</code> with <code>component=circuit_breaker</code>, <code>breaker_name</code>, and <code>degraded_duration</code>. A backend breaker publishes <code>backend.circuit.closed</code> carrying its <code>call</code>.</p><p class="ac-metric">Redis: s3o_circuit_breaker_transitions_total{name, from="open", to="closed"}, s3o_circuit_breaker_state{name} = 0<br>Backend: s3o_backend_call_refused{backend, call} = 0 for each call served again. s3o_circuit_breaker_state{name=&lt;backend&gt;} returns to 0 only once every call is served.</p>'
    },
    RESET: {
      title: 'Reset Failure Counter',
      badge: 'process', badgeText: 'counter reset',
      body: '<p><code>cb.failures = 0</code> &mdash; clears the consecutive failure counter on any outcome this breaker does not count: a success, or an error its filter passes over.</p><p>This happens both during normal closed-state operation (preventing spurious trips from intermittent errors) and after a successful probe (part of the recovery path).</p><p>The counter is also implicitly reset when the circuit transitions to closed, since the next failure sequence starts fresh.</p>'
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

