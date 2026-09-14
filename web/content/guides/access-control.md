---
title: "Setting Up Access Control"
description: "Declare the credential that administers a deployment, onboard a client with only the access it needs, issue an operator credential scoped to the control plane, and rotate either without downtime."
weight: 3
---


This guide walks through setting up access control on a running S3 Orchestrator: the credential that administers the deployment, a client credential narrowed to what one service actually does, and an operator credential that reaches the control plane without reaching object data.

## Overview

There is one credential type - an access key and a secret - and it works on every surface. The same keypair signs S3 requests, signs admin API requests, and logs into the dashboard. What it can do in each is decided by the **grants** its user holds, not by which surface it arrived on.

Three things to hold in mind:

- A credential proves a **user**. It carries no permissions of its own, so two keypairs belonging to one user reach exactly the same things.
- A **grant** pairs that user with a **resource** and a set of **permissions**.
- Every change takes effect on the next request. The registry the request path authenticates against is rebuilt before the command returns.

There are three kinds of resource, and they are three different things rather than three layers of one. A **bucket** is the namespace clients address, `s3://photos/cat.jpg`. A **backend** is a storage provider the orchestrator writes to - your Wasabi account, an R2 bucket, a MinIO node - and one object in a bucket may have copies on several of them at once. The **orchestrator** is the service running in front of both, and is what a grant names for work belonging to no single bucket or provider: reading logs, rotating keys, flushing the cache, creating users.

So granting `bucket:photos` lets someone read and write objects and says nothing about where the bytes live; granting `backend:wasabi-eu` lets them retire that provider and gives them no way to read a single object.

The [access control diagram](../../diagrams/access-control/) shows the whole chain end to end.

## Step 1: Declare the root credential

`auth.root` is the keypair a deployment administers itself with. It is an ordinary user that happens to hold every permission on every resource - there is no separate code path authorizing it, which is exactly why narrower credentials behave predictably.

Generate a pair:

```bash
echo "Access key: AKIA$(openssl rand -hex 8 | tr '[:lower:]' '[:upper:]')"
echo "Secret key: $(openssl rand -base64 30)"
```

Put it in the config file:

```yaml
auth:
  root:
    access_key_id: "${ROOT_ACCESS_KEY_ID}"
    secret_access_key: "${ROOT_SECRET_ACCESS_KEY}"
```

Every value supports `${ENV_VAR}` expansion, so a Vault template, a Nomad template or a Kubernetes secret can supply both halves without the secret touching disk.

{{% notice tip %}}
`auth.root` is required when the dashboard is enabled, because a deployment holding no credential at all would have nothing able to log in and create the first user. A pure S3 endpoint that administers itself through credentials already in its store can leave the stanza out entirely.
{{% /notice %}}

Point the CLI at the deployment. The address can come from the config file, but the keypair never does - it comes from your own flags or environment:

```bash
export S3O_ADMIN_ADDR="https://s3.example.com"
export S3O_ACCESS_KEY_ID="AKIA..."
export S3O_SECRET_ACCESS_KEY="..."

s3-orchestrator admin status
```

## Step 2: Onboard a client with only what it needs

A nightly backup job writes new objects and never reads or deletes them. Granting it the bucket outright would give it `delete` too, so grant the four permissions it uses and leave the rest off.

The user comes first, because a keypair belongs to an identity rather than to a bucket:

```bash
# 1. The identity. The response carries the generated user_id.
s3-orchestrator admin user create -name nightly-backup

# 2. The keypair. This output is the only place the secret ever appears.
s3-orchestrator admin credential issue -user user-abc123 -label "backup job"

# 3. The grant.
s3-orchestrator admin grant add -user user-abc123 \
  -name backups -permissions list-buckets,list,write
```

Capture the secret at step 2. Nothing reads it back: a client that loses it gets a replacement keypair rather than a recovery.

The order matters only in that a credential needs its user first and a grant needs both sides. A user created but not yet granted anything authenticates successfully and reaches nothing, which is a safe intermediate state to leave it in - and a clearer answer for the client than a failed signature would be.

Confirm what the job can now do:

```bash
s3-orchestrator admin grant list
```

`-permissions` defaults to `all`, which is every data-plane permission. On a backend or orchestrator grant, pass `admin-all` for the control-plane equivalent - the two vocabularies do not mix, and `all` on a backend is refused rather than quietly reinterpreted.

### Carving down broad access

A CI system that reads every bucket, except the one holding secrets, is two grants:

```bash
s3-orchestrator admin grant add -user user-ci -name '*' -permissions list-buckets,list,read
s3-orchestrator admin grant add -user user-ci -name secrets -permissions list-buckets
```

A named grant **replaces** the wildcard for the bucket it names rather than adding to it, so `secrets` is visible in a bucket listing and unreadable. The wildcard is expanded against the buckets the deployment declares each time the registry is published, so a bucket created next month is covered without touching the grant.

## Step 3: Issue an operator credential

The control plane has its own vocabulary, and the two do not mix: a bucket cannot be drained, and the orchestrator holds no objects. A grant mixing bucket and `admin-` permissions is refused.

A monitoring sidecar that reads status and nothing else:

```bash
s3-orchestrator admin user create -name monitoring
s3-orchestrator admin credential issue -user user-mon -label "prometheus sidecar"
s3-orchestrator admin grant add -user user-mon -kind orchestrator -permissions admin-read
```

That credential runs `status`, `workers` and `reload-status`, and is refused key rotation, provisioning, log reading and log-level changes alike - the ten `admin-` permissions are genuinely separate, not one flag in ten spellings.

An operator who may retire hardware but must not touch encryption keys:

```bash
s3-orchestrator admin grant add -user user-ops \
  -kind backend -name '*' -permissions admin-drain,admin-decommission
```

`-name '*'` covers every backend including ones added later. Scoping to a single provider instead is `-name wasabi-eu`, and it means what it says: a pass that names no backend runs against the whole fleet, so it is authorized as `backend:*` and an operator granted one provider cannot start a conversion that spends egress on the rest.

{{% notice warning %}}
`admin-provision` is worth withholding unless the role genuinely needs it. A grant carrying it can mint a grant carrying anything, which makes it equivalent to root by a short path.
{{% /notice %}}

## Step 4: Log into the dashboard

The dashboard takes the same credential. Log in at `{ui.path}/login` with an access key and secret - the root keypair, or any credential the store has issued - and the session carries the user that credential proved.

There is no separate dashboard password to rotate, and no third place a credential can be declared.

## Step 5: Rotate without downtime

Several keypairs may name one user, which is what makes rotation an overlap rather than a cutover:

```bash
# 1. Issue the replacement. The old key keeps working.
s3-orchestrator admin credential issue -user user-abc123 -label "app1 rotated 2026-09"

# 2. Move the client onto the new keypair and confirm it works.

# 3. Revoke the old one. It stops working on the next request.
s3-orchestrator admin credential revoke -access-key AKIAOLDKEY
```

No restart and no `SIGHUP`: the registry is rebuilt before each command returns.

Config-declared credentials rotate differently, because the file is their source of truth - edit it and send `SIGHUP`. The root credential rotates the same way, and takes effect on the next request.

## Step 6: Tear down in reverse

Removal is refused while anything still depends on what is being removed, so a script that runs these out of order stops rather than half-completing:

```bash
s3-orchestrator admin grant remove -user user-abc123 -name backups
s3-orchestrator admin credential revoke -access-key AKIAOLDKEY
s3-orchestrator admin user delete -id user-abc123
```

Each refusal names what is in the way and exits non-zero. A bucket that still holds objects will not be dropped either, because doing so would leave those bytes occupying every backend they were written to with no way to reach them.

## Config versus the store

A deployment can declare credentials in two places, and both are live at once.

| | Config file | Store |
|---|---|---|
| Declared by | `buckets[].credentials`, `auth.root` | The provisioning API or the CLI |
| Reaches | Exactly the one bucket that declared it | Any bucket its user is granted |
| Narrowed permissions | No - always full access to that bucket | Yes |
| Changed by | Editing the file, then `SIGHUP` | A command, effective immediately |

Every listing carries a `source` column. Config wins a collision, and the API answers `403` to any attempt to modify a config-declared entry: an operator reading the file has to be able to trust what it says.

Config credentials are the right shape for a bucket whose client is part of the deployment and versioned alongside it. Anything issued to a third party, anything that needs less than full access to its bucket, and anything that should be revocable without an edit belongs in the store.

## See also

- [Access control diagram](../../diagrams/access-control/) - the chain end to end, hover for detail
- [Authentication reference](../../docs/authentication/) - credentials, signatures and presigned URLs
- [CLI reference](../../docs/cli/#bucket-user-credential-and-grant) - every verb and flag
- [Admin API authorization](../../docs/admin-api/#authorization) - the permission each endpoint declares
- [Security hardening](../../docs/security-hardening/) - keeping the root secret off disk
