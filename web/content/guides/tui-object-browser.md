---
title: "Browsing Objects with the TUI"
weight: 8
---


This guide walks through `s3-orchestrator tui`, the built-in terminal object browser. It is a read-only way to explore the object namespace and, for any object, see exactly which backends hold a copy - without leaving the shell or opening the web dashboard.

## Overview

The TUI has two panes:

- **Browser** - a hierarchical listing of the object namespace, one prefix at a time (directories collapse into common prefixes, just like `aws s3 ls`). Large prefixes page in as you scroll.
- **Inspector** - opened on any object, it lists every backend copy of that object with its size, age, encryption status, key id, and content-hash prefix. This is the replica-placement view that makes multi-backend storage legible.

Everything is read-only. The TUI issues `GET` requests to the admin API (`/admin/api/objects` for the listing, `/admin/api/object-locations` for the inspector) and never mutates state.

![The browser listing a prefix of objects and sub-directories](/docs/images/tui-browser-objects.png?classes=lightbox)

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

The browser opens at the root prefix. Move the selection with the arrow keys; open the highlighted row with `enter`.

| Key | Action |
|-----|--------|
| `up` / `down` | Move the selection |
| `enter` / `right` / `l` | Open: descend into a prefix, or open the inspector on an object |
| `backspace` / `left` / `h` | Go up one prefix; from the inspector, return to the listing |
| `/` | Filter the current listing by substring |
| `s` | Cycle the sort order (name / size) |
| `esc` | Clear the filter; from the inspector, return to the listing |
| `r` | Reload the current view |
| `q` / `ctrl+c` | Quit |

![Navigating sub-directories under a prefix](/docs/images/tui-browser-prefixes.png?classes=lightbox)

Long prefixes load lazily - scrolling past the bottom of a truncated page pulls the next batch, so you can walk a bucket with millions of keys without loading it all at once.

To find something in a crowded prefix, press `/` and type - the listing narrows to matching names as you type (the status line shows how many of the loaded rows match), and `esc` clears the filter. Press `s` to cycle the sort order between name and size; directories always sort ahead of objects. Sizes render in binary units (`KiB`, `MiB`, `GiB`).

## Step 3: Inspect an object's copies

Highlight an object (not a directory) and press `enter` to open the inspector. Each row is one backend copy:

```
inspect   photos/2024/img_01.jpg   (2 copies)
BACKEND    SIZE      CREATED    ENC   KEY ID      HASH
minio-a    2516582   2h ago     yes   config-0    9f3a2b1c4d~
minio-c    2516582   2h ago     yes   config-0    9f3a2b1c4d~
```

![The inspector showing an object's two backend copies](/docs/images/tui-inspector.png?classes=lightbox)

Reading the columns:

- **BACKEND** - the backend the copy lives on.
- **SIZE** - stored size in bytes (ciphertext size when the copy is encrypted).
- **CREATED** - how long ago the copy was recorded.
- **ENC** - whether the copy is envelope-encrypted.
- **KEY ID** - the master key that wrapped this copy's data-encryption key.
- **HASH** - a prefix of the plaintext SHA-256, once a hash has been computed.

Two copies with matching sizes and hashes is a healthy replicated object. A single row means the object is under-replicated (or replication is disabled); a mismatch in size or hash across copies is worth investigating.

{{% notice tip %}}
The inspector shows encryption *metadata* only. The wrapped data-encryption key is never sent over the admin API - only the `encrypted` flag and the wrapping `key_id` are exposed.
{{% /notice %}}

## Where it fits

The TUI is the interactive equivalent of `s3-orchestrator admin object-locations -key <key>`. Reach for it when you want to browse rather than look up a single key - for example to confirm replica placement before or after a [drain](../../docs/operations.md), or to spot-check that a newly enabled [replication factor](../replication-guide/) has caught up across backends.
