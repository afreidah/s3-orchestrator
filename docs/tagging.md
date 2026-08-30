---
title: "Object Tagging"
linkTitle: "Object Tagging"
weight: 29
---

Object tags: key/value labels attached to an object independently of its data. Always on, with nothing to configure.

Tags are yours to give meaning to. The one place the orchestrator acts on them itself is [lifecycle expiration](cleanup-and-lifecycle.md#lifecycle-object-expiration), where a rule can filter on a tag; otherwise they are stored, replicated with the object, and served back as written, and no key is treated specially.

## Current status

Every way S3 sets tags is handled: inline on `PutObject` and `CreateMultipartUpload`, the three `?tagging` subresource operations, and `x-amz-tagging-directive` on a server-side copy. `GetObject` and `HeadObject` report how many tags an object carries. An object's set is reachable from the admin API, the CLI and the TUI object inspector, and it is cleared wherever a key stops holding the object it held.

## Tags belong to the object, not to a copy

An object exists as N replicas with no authoritative copy, so a tag set is stored once per key and shared by every replica. Tagging an object tags all of it. There is no way to give two copies of a key different tags, and no request that reaches only one backend.

That also means tagging never touches a provider. The set lives in the metadata store, which avoids three problems at once: provider tagging support is inconsistent, a backend sitting over its usage limit could not be tagged at all, and N replicas could otherwise disagree with nothing to say which one wins.

## Setting tags on write

`PutObject` and `CreateMultipartUpload` accept an `x-amz-tagging` header. It is query-string encoded, not the XML the tagging endpoints exchange:

```bash
aws s3api put-object --bucket photos --key report.pdf --body report.pdf --tagging "team=infra&retain=30d"
```

The header is parsed and validated before the request body is read. An unusable set is refused before any bytes reach a backend, so a rejected write costs no ingress and leaves no orphan to collect.

The tags are written in the object's own transaction. There is no window where the object exists untagged.

## Setting tags on an object that already exists

The `?tagging` subresource carries the three operations, all of which exchange a `Tagging` XML document:

```bash
aws s3api put-object-tagging --bucket photos --key report.pdf --tagging 'TagSet=[{Key=team,Value=infra}]'
aws s3api get-object-tagging --bucket photos --key report.pdf
aws s3api delete-object-tagging --bucket photos --key report.pdf
```

`PutObjectTagging` replaces the whole set rather than merging into it, which is what S3 defines. To add one tag to an object that has three, send all four.

`GetObjectTagging` returns the set sorted by key, so the response is byte-identical run to run. An object carrying no tags answers `200` with an empty `TagSet` rather than `404`: the object is there and simply has nothing on it.

`DeleteObjectTagging` removes the whole set and answers `204`. Clearing a set that is already empty succeeds for the same reason.

A `PutObjectTagging` whose `TagSet` is empty removes every tag, which the spec defines as the same outcome as a delete.

## Knowing whether an object has tags

`GetObject` and `HeadObject` report the size of the set in `x-amz-tagging-count`:

```
x-amz-tagging-count: 3
```

The header is sent only when the object carries at least one tag. An untagged object omits it rather than sending a zero, which is what S3 does and what lets a client read its presence as "there is a set here worth fetching".

It reports how many, never which. `GetObjectTagging` remains the only way to read the tags themselves, and stays the authority: the count is advisory, and a read that cannot reach the metadata store serves the object with the header left off rather than failing over one number.

A cached object carries its count in the cache entry, and a tag write drops the entry so the next read counts again.

## Copying an object

`CopyObject` reads `x-amz-tagging-directive`:

- Absent, or `COPY`, carries the source object's tag set to the destination.
- `REPLACE` ignores the source's tags and takes the set from the copy request's own `x-amz-tagging` header.
- Any other value is refused with `400 InvalidArgument`.

An unrecognised directive is refused rather than quietly treated as `COPY`, because falling back would put the source's tags on a copy the client asked to have different ones.

## Multipart uploads

Tags given to `CreateMultipartUpload` have to survive until the upload completes, which may be hours and many parts later. They are held on the upload row, query-string encoded in the same shape the header uses, and applied to the object that `CompleteMultipartUpload` produces.

The set is validated at create, so an upload that would end in a rejected tag set is refused before any part is transferred rather than after all of them.

Aborting an upload drops the row and its tags with it.

## What clears a tag set

Tags follow the object, not the name. A key that stops holding the object it held stops carrying that object's tags:

- Overwriting a key with a new `PutObject` replaces the tag set with whatever the new request carried. An untagged overwrite leaves the object untagged. Tags do not survive a write that did not ask for them.
- Deleting an object clears its tags, whether by single delete or in a batch.
- Deleting one location of an object that has several does not. Removing the last remaining copy does, because at that point the key no longer holds anything.

## Limits

The AWS tag-set limits, enforced on every path:

- 10 tags per object
- 128 for a tag key
- 256 for a tag value

Lengths count UTF-16 code units, not runes or bytes, because S3 represents tags internally in UTF-16 where a character occupies one or two positions. A key or value of astral-plane characters therefore reaches the limit in half as many characters as a Latin one.

Keys and values are both case sensitive. A key must not be empty, and a set must not repeat one.

## Errors

| Condition | Status | Code |
|-----------|--------|------|
| Key holds no copies | 404 | `NoSuchKey` |
| More than 10 tags | 400 | `BadRequest` |
| Empty key, oversized key or value, duplicate key, undecodable `x-amz-tagging` | 400 | `InvalidTag` |
| `x-amz-tagging-directive` is neither `COPY` nor `REPLACE` | 400 | `InvalidArgument` |

Each message names the offending measurement, so a refusal says which limit was exceeded and by how much rather than a bare "invalid".

`PutObjectTagging` and `DeleteObjectTagging` are recorded in the audit log as `s3.PutObjectTagging` and `s3.DeleteObjectTagging`.

## Reaching tags as an operator

The admin API exposes the set as JSON rather than the S3 XML, since its callers are the CLI, the TUI and the dashboard. See [Admin API](admin-api.md#object-tags).

From the CLI:

```bash
s3-orchestrator admin object-tags -key photos/report.pdf
s3-orchestrator admin object-tags -key photos/report.pdf -tag team=infra -tag retain=30d
s3-orchestrator admin object-tags -key photos/report.pdf -clear
```

`-tag` replaces the whole set, matching `PutObjectTagging`. It is repeatable, and `-clear` and `-tag` are mutually exclusive because they describe different outcomes for the same call.

In the TUI, the object inspector prints the set on a `tags:` line above the per-backend copy table, where it sits outside the table because it describes the object rather than any row in it.

## How they are stored

One row per tag in `object_tags`, keyed `(object_key, tag_key)`, rather than a JSON column on `object_locations`. Filtering objects by tag is a `WHERE tag_key = ? AND tag_value = ?`, which needs an index; a JSON blob turns that into a scan over every object. `idx_object_tags_lookup` on `(tag_key, tag_value)` serves that reverse direction, and the primary key already serves lookup and delete by object key. Ten tags per object caps how many rows a key can add.

There is no foreign key, because there is no table to point at: `object_locations` is keyed `(object_key, backend_name)` and nothing is keyed on object key alone, so `ON DELETE CASCADE` cannot express this. The store clears the rows instead, at every path that puts a new object at a key or removes the last copy of one, inside the same transaction and under the same key lock.

Multipart tags are a `tagging` column on `multipart_uploads` instead. They are only ever read whole for one upload, never filtered by tag, so an index on `(tag_key, tag_value)` would have nothing to serve, and completing or aborting the upload drops the row and the tags together.

See [Database](database.md) for the schema and the migrations that add both.

## Load testing

Two scenarios, measuring different halves:

```bash
make loadtest-tagging LOADTEST_SEED=1000
make loadtest-put-tagged LOADTEST_RATE=200
```

`loadtest-tagging` rotates the three subresource operations over a pre-seeded set, so one run covers the write, the read and the clear. Drive it against a small `LOADTEST_SEED` to concentrate requests on the same keys and show what the per-key lock costs under contention.

`loadtest-put-tagged` is a plain PUT carrying `x-amz-tagging`. Run it against `loadtest-put` at the same rate and size; the delta is what inline tagging adds to the write path.

## See also

- [User Guide](user-guide.md) for tagging from the AWS CLI, rclone, boto3 and the Go SDK
- [Admin API](admin-api.md#object-tags) for the JSON endpoints
- [CLI](cli.md#object-tags) for the `object-tags` subcommand
- [Database](database.md) for the `object_tags` schema
- [Cleanup and lifecycle](cleanup-and-lifecycle.md#lifecycle-object-expiration) for expiring objects by tag
- [Tagging diagram](../diagrams/tagging/) for the write paths and what clears a set
