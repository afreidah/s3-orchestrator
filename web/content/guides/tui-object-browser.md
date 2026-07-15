---
title: "Browsing Objects with the TUI"
weight: 8
---


This guide walks through `s3-orchestrator tui`, the built-in terminal UI. It is a read-only way to explore the object namespace, see exactly which backends hold a copy of an object, and check backend status - without leaving the shell or opening the web dashboard.

## Overview

A persistent left navigation bar switches between sections; the content area to its right renders the active one:

- **Files** - a hierarchical listing of the object namespace, one prefix at a time (directories collapse into common prefixes, just like `aws s3 ls`). Large prefixes page in as you scroll. Opening an object swaps the content area for the **Inspector**, which lists every backend copy of that object with its size, age, encryption status, key id, and content-hash prefix - the replica-placement view that makes multi-backend storage legible.
- **Backends** - one row per configured backend with its circuit-breaker health, drain state, quota usage, object count, and per-period request and transfer counters.

Everything is read-only. The TUI issues `GET` requests to the admin API (`/admin/api/objects` for the listing, `/admin/api/object-locations` for the inspector, `/admin/api/status` for backends) and never mutates state.

![The Files section listing a prefix of objects and sub-directories, with the navigation sidebar](/docs/images/tui-files.png?classes=lightbox)

## Prerequisites

- A running orchestrator instance with the admin API enabled (`ui.admin_token`, or `ui.admin_key` as a fallback - see the [configuration walkthrough](../../docs/configuration.md)).
- The admin token, resolved the same way as the [`admin` subcommand](../../docs/cli.md): flag, then environment, then config file.

## Step 1: Point the TUI at your instance

The TUI resolves the server address and admin token with the precedence **flag &rarr; environment &rarr; config file**. For a local instance the bundled `config.yaml` already carries both, so this is enough:

```bash
s3-orchestrator tui
```

To target a remote instance without a local config, set the environment variables:

```bash
export S3O_ADMIN_ADDR="https://s3.example.com"
export S3O_ADMIN_TOKEN="$(your-secret-tool get admin-token)"
s3-orchestrator tui
```

Or pass them as flags:

```bash
s3-orchestrator tui -addr https://s3.example.com -token "$ADMIN_TOKEN"
```

## Step 2: Navigate the object namespace

The TUI opens on the Files section at the root prefix. Move the selection with the arrow keys; open the highlighted row with `enter`. `tab` moves focus to the sidebar (arrow keys then move the highlight, `enter` opens a section), and `f` / `b` jump straight to Files or Backends.

| Key | Action |
|-----|--------|
| `tab` | Move focus between the sidebar and the content area |
| `f` | Jump to the Files section |
| `b` | Jump to the Backends section |
| `up` / `down` | Move the selection (or the sidebar highlight when it has focus) |
| `enter` / `right` / `l` | Open: a sidebar section, a prefix, or the inspector on an object |
| `backspace` / `left` / `h` | Go up one prefix; from the inspector or Backends, return to where you were |
| `/` | Filter the current listing by substring |
| `s` | Cycle the sort order (name / size) |
| `esc` | Clear the filter; from the inspector or Backends, step back |
| `r` | Reload the current view |
| `q` / `ctrl+c` | Quit |

Long prefixes load lazily - scrolling past the bottom of a truncated page pulls the next batch, so you can walk a bucket with millions of keys without loading it all at once.

To find something in a crowded prefix, press `/` and type - the listing narrows to matching names as you type (the status line shows how many of the loaded rows match), and `esc` clears the filter. Press `s` to cycle the sort order between name and size; directories always sort ahead of objects. Sizes render in binary units (`KiB`, `MiB`, `GiB`).

## Step 3: Inspect an object's copies

Highlight an object (not a directory) and press `enter` to open the inspector. Each row is one backend copy:

```
inspect   photos/2024/img_01.jpg   (2 copies)
BACKEND    SIZE      CREATED    ENC   KEY ID      HASH
minio-a    2.4 MiB   2h ago     yes   config-0    9f3a2b1c4d~
minio-c    2.4 MiB   2h ago     yes   config-0    9f3a2b1c4d~
```

![The inspector showing an object's two backend copies](/docs/images/tui-file-details.png?classes=lightbox)

Reading the columns:

- **BACKEND** - the backend the copy lives on.
- **SIZE** - stored size in binary units (ciphertext size when the copy is encrypted).
- **CREATED** - how long ago the copy was recorded.
- **ENC** - whether the copy is envelope-encrypted.
- **KEY ID** - the master key that wrapped this copy's data-encryption key.
- **HASH** - a prefix of the plaintext SHA-256, once a hash has been computed.

Two copies with matching sizes and hashes is a healthy replicated object. A single row means the object is under-replicated (or replication is disabled); a mismatch in size or hash across copies is worth investigating.

{{% notice tip %}}
The inspector shows encryption *metadata* only. The wrapped data-encryption key is never sent over the admin API - only the `encrypted` flag and the wrapping `key_id` are exposed.
{{% /notice %}}

## Step 4: Check backend status

Press `b` (or select **Backends** in the sidebar) to switch to the status view. Each row is one configured backend:

```
backends   3 configured   db: healthy   usage period: 2026-07
BACKEND    HEALTH     DRAIN     USED       LIMIT      OBJECTS   API      INGRESS    EGRESS
minio-a    healthy    -         2.4 GiB    10.0 GiB   1284      9021     4.7 GiB    2.8 GiB
minio-b    healthy    draining  8.9 GiB    10.0 GiB   4102      512      1.0 GiB    3.1 GiB
minio-c    unhealthy  -         0 B        -          0         0        0 B        0 B
```

![The Backends section listing per-backend health and usage](/docs/images/tui-backends.png?classes=lightbox)

Reading the columns:

- **HEALTH** - the backend's circuit-breaker state; `unhealthy` means the breaker has tripped and the backend is being skipped.
- **DRAIN** - `draining` while a drain is evacuating the backend, otherwise `-`.
- **USED** / **LIMIT** - quota bytes used against the configured limit (`-` when no limit is set).
- **OBJECTS** - object copies the backend holds.
- **API** / **INGRESS** / **EGRESS** - request count and bytes transferred for the current usage period, shown in the title bar.

The title bar also reports the metadata database health and the usage period the counters cover. Press `r` to refresh the snapshot. This is the interactive equivalent of `s3-orchestrator admin status`.

## Where it fits

The TUI is the interactive equivalent of `s3-orchestrator admin object-locations -key <key>`. Reach for it when you want to browse rather than look up a single key - for example to confirm replica placement before or after a [drain](../../docs/operations.md), or to spot-check that a newly enabled [replication factor](../replication-guide/) has caught up across backends.
