---
title: "CLI Subcommands"
linkTitle: "CLI Subcommands"
weight: 32
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

Full-screen, read-only terminal UI. Launches an interactive [Bubble Tea](https://github.com/charmbracelet/bubbletea) app with a persistent left navigation bar: **Files** browses the object namespace one prefix at a time and, on any object, opens an inspector pane showing every backend copy; **Backends** shows the configured backends and their live status; **Replication** shows a self-refreshing view of replication health; **Logs** shows recent structured log entries. The pane with keyboard focus is shown with a bright title bar (the other is muted). Resolves the server address and admin token with the same precedence as `admin` (**flag &rarr; environment &rarr; config file**), loading `config.yaml` only when a value is still missing:

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
| `l` | Jump to the Logs section |
| `L` | Cycle the Logs level filter (all / INFO / WARN / ERROR) |
| `R` | Reconcile usage counters across all backends (asks to confirm) |
| `F` | Flush the in-memory object cache (asks to confirm) |
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

The inspector renders one row per backend copy - backend, size, age, whether the copy is encrypted, its key id, and a content-hash prefix - sourced from `GET /admin/api/object-locations`. It is the interactive equivalent of `admin object-locations`, and like the rest of the admin surface it never displays raw key material.

![The TUI inspector showing an object's backend copies](/docs/images/tui-file-details.png)

The **Backends** section is the interactive equivalent of `admin status`, sourced from `GET /admin/api/status`. It renders one row per configured backend - circuit-breaker health, drain state, quota used and limit, a `USE%` column (used / limit), object count, and the current period's API request, ingress, and egress counters. A stats line under the title shows the metadata database health (green when healthy, red when not) and the total usage across backends (`used / limit (pct%)`, coloured by fill). Press `r` to refresh the snapshot.

The metadata database health is also shown persistently at the bottom of the sidebar (`db ok` green / `db DOWN` red), fetched at startup so it is visible from every section.

The **Replication** section shows cluster-wide replication health, sourced from `GET /admin/api/replication` - the configured replication factor and the current under-replicated and over-replicated object counts, with the age of the underlying snapshot. It auto-refreshes every few seconds while it is the active section (the counts drift constantly as workers reconcile), so the view stays live without a keypress; the ticker stops once you leave. The pending counts are coloured amber when there is a backlog and green at zero. Press `r` to force an immediate refresh. Because the endpoint reads a snapshot the metrics collector already computes on its own interval, polling it is cheap.

The **Logs** section shows recent structured log entries from the instance's in-memory log buffer, sourced from `GET /admin/api/logs` - the same buffer the web dashboard's logs pane reads. Each row is time, level, component, and a human-readable message with its structured attributes appended as `key=value` pairs (not raw JSON). The level is colour-coded by severity (WARN and ERROR stand out; INFO stays neutral). Press `L` to cycle the minimum-level filter (all / INFO / WARN / ERROR) and `r` to refresh.

Beyond browsing, the TUI can trigger a growing set of **admin actions**. Every write action shows a `y/N` confirmation in the footer before it runs, and its result (or error) is reported there afterwards. The instance-wide actions available from any section are `R` (reconcile usage counters) and `F` (flush the object cache); more will land over time.

![The TUI Logs section showing recent structured log entries](/docs/images/tui-logs.png)

![The TUI Backends section showing per-backend status](/docs/images/tui-backends.png)

## Importing Existing Data

The `sync` subcommand imports objects from an existing backend bucket into the orchestrator's metadata database. Use this when bringing a bucket that already has data under orchestrator management.

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

The `--bucket` flag specifies which virtual bucket the imported objects belong to. Keys are stored internally as `{bucket}/{key}`, so this determines the namespace.

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
| `--bucket` | (required) | Virtual bucket name to assign to imported objects |
| `--prefix` | `""` | Only sync objects with this key prefix |
| `--dry-run` | `false` | Preview without writing to the database |

