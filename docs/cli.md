---
title: "CLI Subcommands"
linkTitle: "CLI Subcommands"
weight: 33
---

# CLI Subcommands


### version

Prints the binary version, Go version, and platform:

```bash
s3-orchestrator version
# s3-orchestrator v0.41.7 go1.26.0 linux/amd64
```

### validate

Validates a configuration file without starting the server. Exits 0 on success with a brief summary, or exits 1 with error details. Useful for CI pipelines or pre-deploy checks:

```bash
s3-orchestrator validate -config config.yaml
```

### admin

Operational CLI for inspecting and controlling a running instance. Resolves the server address and admin token with the precedence **flag &rarr; environment &rarr; config file**, loading `config.yaml` only when a value is still missing. This lets a local binary target a remote instance with just env vars and no server config:

```bash
export S3O_ADMIN_ADDR="https://s3.example.com"
export S3O_ADMIN_TOKEN="$(your-secret-tool get admin-token)"
s3-orchestrator admin usage-reconcile
```

```bash
s3-orchestrator admin [flags] <command>
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `config.yaml` | Path to config file; loaded only when `-addr`/`-token` (or their env vars) are unset |
| `-addr` | `$S3O_ADMIN_ADDR`, else config `server.listen_addr` | Server address |
| `-token` | `$S3O_ADMIN_TOKEN`, else config `ui.admin_token` / `ui.admin_key` | Admin API token |
| `-json` | off | Emit raw JSON instead of human-readable text |

**Output format:**

Commands render human-readable text by default. Pass `-json` for the raw JSON
the server returns, suitable for scripting (`jq`, etc.):

```bash
s3-orchestrator admin status            # human-readable summary
s3-orchestrator admin -json status      # raw JSON for scripts
```

> **Migration note:** the default output is human-readable text. Earlier
> versions printed JSON unconditionally; scripts that parse stdout must add
> `-json` to keep the JSON contract.

**Streaming progress:**

The long-running commands stream per-item progress as they work rather than
blocking on a single final payload. In text mode each item renders on one line,
dotted out to its status and per-item duration; a final line summarizes the run.
`-json` mode emits one JSON object per line (NDJSON): a `start` event, a
`step_start`/`step_end` pair (or a single `step_end` for concurrent ops) per
item, and a terminal `result`.

| Command | Per-item verb | Item |
|---------|---------------|------|
| `rebalance` | `moving` | object key and the backends it moves between |
| `backfill-checksums` | `hashing` | object key |
| `scrub` | `verifying` | object key |
| `reconcile` | `reconciling` | backend |
| `replicate` | `replicating` | object key |
| `over-replication --execute` | `removing` | object key |
| `remove-backend --purge --confirm` | `deleting` | object key |

```text
$ s3-orchestrator admin backfill-checksums
backfill-checksums started
  hashing photos/a.jpg ............................. OK     (12ms)
  hashing photos/b.jpg ............................. OK     (9ms)
done: processed 2 (1.5s)
```

`replicate` and `over-replication` fan their work out across a worker pool, so
each item prints as one complete line when it finishes (no live dots) to keep
concurrent output from interleaving.

**Example output:**

`reconcile` dots each backend out to its result and prints a summary line. Run
it twice in a row and the second pass should converge toward a no-op:

```text
$ s3-orchestrator admin reconcile
reconcile started
  reconciling aws-east ........................... OK     (335ms)
  reconciling backblaze .......................... OK     (510ms)
  reconciling wasabi ............................. OK     (162ms)
  reconciling minio .............................. OK     (261ms)
done: imported 32, removed 4 across 4 backend(s) (1.3s)
```

Scope it to a single backend with `-backend`:

```text
$ s3-orchestrator admin reconcile -backend backblaze
reconcile started
  reconciling backblaze .......................... OK     (510ms)
done: imported 0, removed 0 across 1 backend(s) (0.5s)
```

`replicate` reports how many missing replicas it created:

```text
$ s3-orchestrator admin replicate
replicate started
done: created 0 copies (12ms)
```

`usage-reconcile` lists the per-backend byte adjustments it applied to
`bytes_used`, or an empty list when the ledger already matches:

```text
$ s3-orchestrator admin usage-reconcile
adjustments:
  aws-east: -3923096188
  backblaze: -612311667
  wasabi: -452341834
status: reconciled

$ s3-orchestrator admin usage-reconcile   # already consistent
adjustments:
status: reconciled
```

**Commands:**

```bash
# Show backend health, usage, and circuit breaker state
s3-orchestrator admin status

# List all copies of an object across backends
# (s3-orchestrator tui browses the same data interactively)
s3-orchestrator admin object-locations -key "my-bucket/path/to/file.txt"

# Show cleanup queue depth and pending items
s3-orchestrator admin cleanup-queue

# Force flush usage counters to the database
s3-orchestrator admin usage-flush

# Drop every entry from the in-memory object data cache
# (returns 503 when caching is disabled in config)
s3-orchestrator admin cache-flush

# Inspect cache size and entry count
s3-orchestrator admin cache-stats

# Drop a single key from the cache
s3-orchestrator admin cache-invalidate -key bucket/path/object.txt

# Drop every cached key under a prefix
s3-orchestrator admin cache-invalidate-prefix -prefix bucket/path/

# Trigger one replication cycle (creates missing replicas)
s3-orchestrator admin replicate

# Trigger one rebalance cycle (redistribute objects across backends per the
# configured strategy; falls back to "spread" with defaults when unconfigured)
s3-orchestrator admin rebalance

# Show count of over-replicated objects
s3-orchestrator admin over-replication

# Clean over-replicated objects (remove excess copies)
s3-orchestrator admin over-replication --execute

# Clean with a custom batch size
s3-orchestrator admin over-replication --execute --batch-size 200

# View the current log level
s3-orchestrator admin log-level

# Change log level at runtime (no restart or SIGHUP needed)
s3-orchestrator admin log-level -set debug

# Start draining a backend (migrates all objects to other backends)
s3-orchestrator admin drain <backend-name>

# Check drain progress
s3-orchestrator admin drain-status <backend-name>

# Cancel an active drain (objects already moved are not rolled back)
s3-orchestrator admin drain-cancel <backend-name>

# Remove a backend's database records (S3 objects preserved, reversible via sync)
s3-orchestrator admin remove-backend <backend-name>

# Preview what --purge would destroy (dry-run)
s3-orchestrator admin remove-backend <backend-name> --purge

# Remove a backend AND delete its S3 objects (requires --confirm)
s3-orchestrator admin remove-backend <backend-name> --purge --confirm

# Encrypt all unencrypted objects in-place (requires encryption enabled)
s3-orchestrator admin encrypt-existing

# Decrypt all encrypted objects back to plaintext (requires encryption enabled for key access)
s3-orchestrator admin decrypt-existing

# Re-wrap all DEKs encrypted with a specific key ID (key rotation)
s3-orchestrator admin rotate-encryption-key --old-key-id config-0

# Trigger an on-demand integrity scrub cycle (verify stored hashes)
s3-orchestrator admin scrub

# Scrub with a custom batch size
s3-orchestrator admin scrub -batch-size 500

# Verify every copy of one object now, without waiting for its turn in the queue
s3-orchestrator admin scrub -key photos/2024/beach.jpg

# Compute and store content hashes for all unhashed objects
s3-orchestrator admin backfill-checksums

# Backfill with a custom batch size (objects fetched per pass)
s3-orchestrator admin backfill-checksums -batch-size 50

# Bound a single run and pace it so it fits the client timeout and
# doesn't hammer backends: process at most 500 objects, pausing 250ms
# between batches. The response reports "done" once the backlog drains;
# re-run until done.
s3-orchestrator admin backfill-checksums -max 500 -delay-ms 250

# Reconcile all backends (import untracked objects, remove stale DB entries)
s3-orchestrator admin reconcile

# Reconcile a single backend
s3-orchestrator admin reconcile -backend backblaze

# Show background worker last-tick health (503 in proxy-only mode)
s3-orchestrator admin workers

# Show the outcome of the last SIGHUP config reload
s3-orchestrator admin reload-status

# Download the flight-recorder trace ring buffer to a file for `go tool trace`
# (requires debug.flight_recorder.enabled; -o sets the output path)
s3-orchestrator admin trace-snapshot -o trace.bin
```

The admin API requires `ui.admin_token` (or `ui.admin_key` as fallback) to be set in the configuration. All requests are authenticated via the `X-Admin-Token` header.

### tui

Full-screen terminal UI. Launches an interactive [Bubble Tea](https://github.com/charmbracelet/bubbletea) app with a persistent left navigation bar: **Files** browses the object namespace one prefix at a time and, on any object, opens an inspector pane showing every backend copy; **Backends** shows the configured backends and their live status; **Replication** shows a self-refreshing view of replication health; **Workers** shows each background service's last-tick health; **Cleanup** shows the cleanup queue and its dead-letter table; **Cache** shows the object data cache's utilization and hit rate; **Logs** shows recent structured log entries; **Ops** runs admin write actions. The pane with keyboard focus is shown with a bright title bar (the other is muted). Resolves the server address and admin token with the same precedence as `admin` (**flag &rarr; environment &rarr; config file**), loading `config.yaml` only when a value is still missing:

```bash
export S3O_ADMIN_ADDR="https://s3.example.com"
export S3O_ADMIN_TOKEN="$(your-secret-tool get admin-token)"
s3-orchestrator tui
```

![The TUI Files section browsing a prefix](/docs/images/tui-files.png)

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `config.yaml` | Path to config file; loaded only when `-addr`/`-token` (or their env vars) are unset |
| `-addr` | `$S3O_ADMIN_ADDR`, else config `server.listen_addr` | Server address |
| `-token` | `$S3O_ADMIN_TOKEN`, else config `ui.admin_token` / `ui.admin_key` | Admin API token |

**Keys:**

| Key | Action |
|-----|--------|
| `tab` | Move focus between the sidebar and the content area |
| `f` | Jump to the Files section |
| `b` | Jump to the Backends section |
| `p` | Jump to the Replication section |
| `w` | Jump to the Workers section |
| `u` | Jump to the Cleanup section |
| `c` | Jump to the Cache section |
| `l` | Jump to the Logs section |
| `o` | Jump to the Ops section |
| `L` | Cycle the Logs level filter (all / INFO / WARN / ERROR) |
| `t` | In Cleanup, switch between the pending and dead-letter listings |
| `R` | In Cleanup's dead-letter listing, requeue the selected row's backend; in Backends, reconcile the selected backend (asks to confirm) |
| `d` | In Backends, drain the selected backend (asks to confirm) |
| `Q` | In Backends, requeue the selected backend's dead-lettered cleanups (asks to confirm) |
| `x` | In Backends, cancel the drain the pane is following (asks to confirm) |
| `D` | In Files, download the selected object to a prompted local path |
| `U` | In Files, upload a local file under a prompted key |
| `X` | In Files, delete the selected object, or everything under the selected directory (asks to confirm) |
| `S` | In the inspector, verify every copy of the object now (asks to confirm) |
| `y` / `n` | Accept / cancel a pending action confirmation |
| `up` / `down` | Move the selection (or the sidebar highlight when it has focus) |
| `enter` / `right` / `l` | Open: a sidebar section, a prefix, or the inspector on an object |
| `backspace` / `left` / `h` | Go up one prefix; from the inspector or Backends, return to where you were |
| `/` | Filter the current listing by substring |
| `s` | Cycle the sort order (name / size) |
| `esc` | Clear the filter; from the inspector or Backends, step back |
| `r` | Reload the current view |
| `q` / `ctrl+c` | Quit |

The listing pages lazily: scrolling past the bottom of a truncated prefix pulls the next page. Press `/` to filter the loaded rows by substring, and `s` to sort by name or size. Objects show their stored size in human-readable units alongside child prefixes.

The inspector renders one row per backend copy - backend, size, age, whether the copy is encrypted, its key id, a content-hash prefix, and how long ago that hash was last checked against the stored bytes - sourced from `GET /admin/api/object-locations`. It is the interactive equivalent of `admin object-locations`, and like the rest of the admin surface it never displays raw key material.

A copy reading `never` under `VERIFIED` has a recorded hash that nothing has ever read back, which is not the same as a copy known to be intact. Press `S` to verify every copy of the object immediately rather than waiting for the scrub queue to reach it; the footer reports how many passed and names any that did not. A copy whose bytes do not match its hash is discarded and rebuilt from a healthy one, exactly as a scrub pass would, so the confirmation says so.

![The TUI inspector showing an object's backend copies](/docs/images/tui-file-details.png)

The **Backends** section is the interactive equivalent of `admin status`, sourced from `GET /admin/api/status`. It renders one row per configured backend - circuit-breaker health, drain state, quota used and limit, a `USE%` column (used / limit), object count, and the current period's API request, ingress, and egress counters. A stats line under the title shows the metadata database health (green when healthy, red when not) and the total usage across backends (`used / limit (pct%)`, coloured by fill). Press `r` to refresh the snapshot.

Three admin actions act on the highlighted row, each behind a confirmation naming that backend so a keystroke on the wrong row cannot start work on it: `d` drains every copy off it, `R` reconciles metadata against its storage, and `Q` requeues its dead-lettered cleanups. Backend removal is deliberately not here - it is irreversible with `purge=true` and stays an `admin remove-backend` operation, behind its two-phase confirmation.

A drain runs for as long as it takes, so the pane follows it: once accepted, a line under the stats reports the objects moved and what remains, refreshed every couple of seconds until the drain finishes or is cancelled. Press `x` to cancel the drain the pane is following; copies already moved stay moved. The key is only offered while a drain is in flight.

The metadata database health is also shown persistently at the bottom of the sidebar (`db ok` green / `db DOWN` red), fetched at startup so it is visible from every section.

The **Replication** section shows cluster-wide replication health, sourced from `GET /admin/api/replication` - the configured replication factor and the current under-replicated and over-replicated object counts, with the age of the underlying snapshot. It auto-refreshes every few seconds while it is the active section (the counts drift constantly as workers reconcile), so the view stays live without a keypress; the ticker stops once you leave. The pending counts are coloured amber when there is a backlog and green at zero. Press `r` to force an immediate refresh. Because the endpoint reads a snapshot the metrics collector already computes on its own interval, polling it is cheap.

The **Workers** section shows every registered background service's last-tick health, sourced from `GET /admin/api/workers` - last success, last failure, consecutive failure count, and the last error. A worker that is running but failing every tick looks identical to a healthy one in `/health`, so this is where that difference surfaces; the title bar reports how many services are currently failing. A proxy-only deployment registers no worker pool, and the pane says so rather than reporting an error.

The **Cleanup** section shows the cleanup queue and its dead-letter table, sourced from `GET /admin/api/cleanup-queue` and `GET /admin/api/cleanup-dlq`. Both listings share the pane, toggled with `t`, and the title bar carries both depths so a backlog in the listing you are not looking at stays visible. The depth is the true total; the listing itself is one page of it. On the dead-letter listing, `R` requeues every dead-lettered row for the selected row's backend (`POST /admin/api/cleanup-dlq/requeue`) - a whole-backend operation, which is what the confirmation names, and the pane reloads afterwards so the depths stay honest.

The **Cache** section shows the object data cache, sourced from `GET /admin/api/cache` - entry count, bytes held against the configured maximum, and the lifetime hit rate. The hit rate is coloured for a cache doing its job rather than a full one: green at 60% and above, amber down to 25%, red below. A cache nothing has read yet says so instead of reporting 0%, which would read as a broken cache. When object caching is disabled the endpoint answers 503 and the pane reports that as configuration, not failure.

The **Logs** section shows recent structured log entries from the instance's in-memory log buffer, sourced from `GET /admin/api/logs` - the same buffer the web dashboard's logs pane reads. Each row is time, level, component, and a human-readable message with its structured attributes appended as `key=value` pairs (not raw JSON). The level is colour-coded by severity (WARN and ERROR stand out; INFO stays neutral). Press `L` to cycle the minimum-level filter (all / INFO / WARN / ERROR) and `r` to refresh.

Beyond browsing, the TUI can trigger a growing set of **admin actions**. Every write action shows a `y/N` confirmation before it runs, and its result (or error) is reported afterwards. Instance-wide actions live on the Ops menu; an action that targets one row, such as the Cleanup pane's requeue, lives on the pane that shows the row.

The **Files** section acts on objects as well as browsing them. `D` downloads the highlighted object to a prompted local path, `U` uploads a local file under a prompted key, and `X` removes the highlighted row. Both prompts start filled with a sensible answer - the object's base name, or the current prefix plus the file's name - so the usual case is a keystroke away and anything else is an edit.

Deleting a directory removes everything under it, so the confirmation states how many objects that is rather than just naming the prefix: the pane counts a page of keys first and asks "Delete 128 objects under bucket/photos/?". A prefix with more objects than one page holds reads as "at least 1,000", since understating what a delete will remove is worse than being vague about the total.

Downloads and uploads move real bytes, so the status line reports the progress (`downloading bucket/db.sql   45.2 MiB / 120.0 MiB (37%)`) while the interface stays responsive. A download writes to a temporary file beside its destination and renames it into place only once the whole body has landed, so an interrupted transfer leaves nothing where a complete file would be - a failure reports what went wrong rather than leaving a truncated file to be discovered later.

The **Ops** section, reached with `o`, is the full menu, grouped by kind: the maintenance passes (rebalance, replicate under-replicated objects, clean over-replicated copies, scrub, backfill checksums, reconcile metadata, reconcile usage counters, flush usage counters to the database), cache control (flush the cache, invalidate one key, invalidate a prefix), and the encryption transitions (encrypt existing, decrypt existing, rotate the encryption key). Accepting the confirmation switches to a scrolling output pane immediately, so an operation that takes minutes reports that it started rather than leaving the menu live until it finishes.

An entry ending in `...` asks for a value first: the key or prefix to invalidate, or the key id to rotate away from. Type it and press `enter`, or `esc` to abandon the action. An empty value is refused in the prompt rather than sent, since the endpoints that take one reject an empty value on purpose. Rotation still confirms after the key id is entered; cache invalidation does not, because the value typed is itself the statement of intent and nothing is lost but a cached copy.

The long-running operations render the same live progress stream `admin` shows on the command line, one line per item - a rebalance names each object and the backends it travelled between. The rest finish in a single round trip and report what changed in the operation's own terms: `dropped 1,204 cache entries`, `corrected 2 backends: e2 -162.1 MiB, oci +4.1 MiB`, `encrypted 1,200 objects, 3 failed`. An operation that did not run - a rebalance already within threshold, a feature not configured - says so instead of reporting a false zero. Press `esc` to step back to the menu, then to the nav.

![The TUI Logs section showing recent structured log entries](/docs/images/tui-logs.png)

![The TUI Backends section showing per-backend status](/docs/images/tui-backends.png)

## Importing Existing Data

The `sync` subcommand imports objects from an existing backend bucket into the orchestrator's metadata database, on demand, without waiting for the reconcile cycle to reach that backend.

Objects are recorded at the key the backend actually holds them under. An object already under a virtual bucket's prefix (`my-files/photos/cat.jpg`) is imported as a managed object and behaves like any other. An object outside every configured prefix (`cat.jpg` at the bucket root) is recorded as **unmanaged**: it counts toward the backend's quota, because the bytes are really there and placement decisions depend on accurate totals, but replication, rebalance, drain and integrity passes leave it alone. It is also not reachable through the S3 API, since no virtual bucket claims it.

Importing raw data at the bucket root therefore makes the orchestrator *aware* of it, not responsible for it. To bring such objects under management, move them under a virtual bucket's prefix on the backend first, then sync.

### Encrypted objects

Import reads the start of each object to see whether it is an orchestrator encryption envelope, because recording ciphertext as plaintext would make the read path serve raw encrypted bytes to clients.

An envelope needs the key that encrypted it. Every write mints its own key, so a matching object key is not enough to prove a stray copy belongs to the row the ledger still holds: import adopts an existing copy's key only when the object's header shows the two came from the same encryption. That is the normal case for a replica whose row was lost, and it is imported fully readable.

An envelope no surviving row can decrypt is recorded as encrypted with no key. It counts toward quota, but reads of that copy fail rather than returning ciphertext, and it is counted under `s3o_import_classified_total{decision="unreadable"}`. Restore those objects from another source or delete them; the key is gone.


### Dry run first

Always preview what would be imported before committing:

```bash
s3-orchestrator sync \
  --config config.yaml \
  --backend oci \
  --bucket my-files \
  --dry-run
```

### Run the import

```bash
s3-orchestrator sync \
  --config config.yaml \
  --backend oci \
  --bucket my-files
```

`--bucket` records which virtual bucket the import was run for and appears in the log. It does not affect the keys written: those come from the backend listing verbatim, and whether an object is managed is decided by the prefixes of every configured bucket.

### Partial import with --prefix

Import only objects under a specific key prefix:

```bash
s3-orchestrator sync \
  --config config.yaml \
  --backend oci \
  --bucket my-files \
  --prefix "photos/"
```

Objects already tracked in the database for that backend are automatically skipped. The command logs per-page progress and a final summary.

### Sync flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config` | `config.yaml` | Path to configuration file |
| `--backend` | (required) | Backend name to sync from |
| `--bucket` | `""` | Virtual bucket the import is being run for; recorded in the log, not used to build keys |
| `--prefix` | `""` | Only sync objects with this key prefix |
| `--dry-run` | `false` | Preview without writing to the database |

