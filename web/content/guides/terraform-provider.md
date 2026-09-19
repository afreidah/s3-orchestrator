---
title: "Provisioning with Terraform"
description: "Declare the buckets, users, keypairs and grants a deployment serves, onboard a client from one map entry, narrow access without a gap, and move a bucket out of the configuration file without downtime."
weight: 4
---


The [access control guide](../access-control/) onboards a client by hand, one admin command at a time. This one does the same work declaratively: a Terraform provider that manages the identities a running S3 Orchestrator authorizes requests against, so the access a deployment grants is a file in version control rather than a sequence of commands somebody remembers running.

## What it manages, and what it does not

The provider manages exactly what the admin API manages: the **buckets** a deployment serves, the **users** that reach them, the **credentials** that prove those users, and the **grants** that say what each one may do.

Backends are absent, and deliberately so. There is no endpoint that creates one, and a backend's structural fields are read once at startup, so a resource whose apply silently required a process restart would be worse than no resource. Backends stay with whatever templates the configuration file. The same reasoning covers configuration generally: Terraform models declarative state a running process can adopt, and most of that file is read once at boot.

Anything the configuration file declares is read-only through the API - `s3o admin bucket list` and `s3o admin user list` show those with a `config` source. The provider refuses to manage or import one and says which it is, rather than letting the refusal read as a credential problem. For buckets that is a starting point rather than a dead end; [moving a bucket out of the configuration file](#moving-a-bucket-out-of-the-configuration-file) below does it without downtime.

## Installing the provider

The provider is published as `afreidah/s3-orchestrator` to both the [Terraform Registry](https://registry.terraform.io/providers/afreidah/s3-orchestrator/latest/docs) and the [OpenTofu Registry](https://search.opentofu.org/provider/afreidah/s3-orchestrator/latest), so `terraform init` or `tofu init` finds it with no further setup. Either link carries the generated reference for every resource and data source, argument by argument. Pin it the way you pin any provider:

```hcl
terraform {
  required_providers {
    s3orchestrator = {
      source  = "afreidah/s3-orchestrator"
      version = "~> 0.147"
    }
  }
}
```

The provider shares the orchestrator's version numbering - both come from one repository - but it is published only when it changes, so its versions are a subset of the orchestrator's releases. Pin it to the version that introduced what you use rather than to whatever the deployment runs.

That distinction matters when a resource is newer than the deployment it talks to. `s3orchestrator_bucket` needs 0.147 or later on both sides; against an older deployment the provider calls an endpoint that is not there and gets a 404 rather than a message about versions.

To work against an unreleased build instead, point Terraform at a locally built binary through a development override in `~/.terraformrc` or a file named by `TF_CLI_CONFIG_FILE`:

```hcl
provider_installation {
  dev_overrides {
    "afreidah/s3-orchestrator" = "/path/to/s3-orchestrator/terraform/terraform-provider-s3-orchestrator"
  }
  direct {}
}
```

With an override in place Terraform skips `init` for this provider and prints a warning on every plan saying so. That warning is the confirmation the override took effect, not a problem to fix.

## Configuring the provider

The provider signs its requests with SigV4, exactly as the admin CLI and the dashboard do. There is no separate provider credential: it is an ordinary keypair whose user holds the admin grants for what you ask it to do. `admin-provision` covers every resource here.

Each attribute falls back to the environment variable the admin CLI already reads, so a shell configured for `s3o admin` is configured for Terraform:

| Attribute | Environment variable |
|-----------|----------------------|
| `address` | `S3O_ADMIN_ADDR` |
| `access_key_id` | `S3O_ACCESS_KEY_ID` |
| `secret_access_key` | `S3O_SECRET_ACCESS_KEY` |

Naming the address in the block instead pins a configuration to one deployment, which is worth doing for anything that must never reach production by accident. Leave the keypair in the environment either way - a literal secret in a `.tf` file is a secret in version control.

```hcl
provider "s3orchestrator" {
  address = "https://s3.example.com"
}
```

## Declaring a bucket

A bucket is the namespace the orchestrator accepts writes under. Declaring one does not create a bucket on any backend; which backends a deployment writes to, and what credentials reach them, stays configuration.

```hcl
resource "s3orchestrator_bucket" "photos" {
  name                  = "photos"
  max_multipart_uploads = 4
}
```

`max_multipart_uploads` caps how many multipart uploads may be in flight against the bucket at once; zero, the default, is unlimited. Browser access takes repeated `cors_rule` blocks:

```hcl
resource "s3orchestrator_bucket" "photos" {
  name = "photos"

  cors_rule {
    allowed_origins = ["https://app.example.com"]
    allowed_methods = ["GET", "HEAD"]
    expose_headers  = ["ETag"]
    max_age         = 600
  }
}
```

The rules are validated by the deployment rather than by the provider, so a rule that cannot match anything - no origin, no method, two wildcards in one origin - is refused at apply with the reason. A rule that stored cleanly and then failed every later reload would take the fleet's reloads down with it.

Changing the limit or the rules is an update in place. Changing the name replaces the bucket, because the name identifies every object stored beneath it. That asymmetry is deliberate: the orchestrator refuses to delete a bucket holding objects or named by a grant, so a limit modelled as a replacement would fail on every bucket worth having.

Dropping a `cors_rule` block removes that rule. The configuration is the whole of what the bucket carries, not a set of additions to it.

Deleting the resource removes the bucket, and is refused while anything is stored under it. Emptying a bucket stays a deliberate act rather than something an apply does on your behalf.

## Onboarding one client

Three resources, in the order the model requires: an identity, a keypair that proves it, and a grant that empowers it.

```hcl
resource "s3orchestrator_user" "backup" {
  name = "temporal-backup-job"
}

resource "s3orchestrator_credential" "backup" {
  user_id = s3orchestrator_user.backup.id
  label   = "vault-managed"
}

resource "s3orchestrator_grant" "backup" {
  user_id     = s3orchestrator_user.backup.id
  name        = "photos"
  permissions = ["list", "read", "write"]
}
```

A user created without grants authenticates and reaches nothing, which is a safe state to leave one in while you decide what it should reach.

One user per process, not per bucket. A job that reads from two buckets is still one identity holding two grants; giving it two users means rotating two keypairs.

## Minting a keypair, or supplying one

Omitting both halves above has the orchestrator mint a keypair. That works, but the minted secret crosses the wire exactly once and lives in Terraform state afterwards - **losing state loses the credential**, because nothing reads a secret back out of the orchestrator.

Supplying both halves registers a keypair generated somewhere else, which is how this composes with a secret manager: the secret is created where secrets are created, and the orchestrator is only told about it.

```hcl
resource "s3orchestrator_credential" "backup" {
  user_id           = s3orchestrator_user.backup.id
  label             = "vault-managed"
  access_key_id     = vault_kv_secret_v2.backup.data["access_key_id"]
  secret_access_key = vault_kv_secret_v2.backup.data["secret_access_key"]
}
```

Supplying one half without the other is refused at plan time rather than during the apply, because a key with no secret cannot sign and a secret with no key names nothing.

Several credentials may name one user, which is what makes a rotation overlap rather than a cutover: add the new keypair, move the client onto it, then remove the old resource.

## Onboarding several clients

Repeating those three resources per client gets tedious quickly. The repository ships a module that takes a map of identities and fans them out, flattening each identity's grants into stable keys:

```hcl
module "identities" {
  source = "github.com/afreidah/s3-orchestrator//terraform/modules/s3-orchestrator"

  buckets = {
    "unified"   = {}
    "artifacts" = {}
  }

  identities = {
    "temporal-backup-job" = {
      label = "temporal backups"
      grants = [
        { name = "unified", permissions = ["list", "read", "write"] },
        { name = "artifacts", permissions = ["read"] },
      ]
    }
    "aptly" = {
      grants = [
        { name = "artifacts", permissions = ["list", "read", "write"] },
      ]
    }
  }
}

output "keypairs" {
  value     = module.identities.keypairs
  sensitive = true
}
```

Adding a client becomes one map entry rather than a deploy, and so does adding a bucket. The module orders the two, because a grant naming a bucket nothing declares is refused and the name in a grant is a literal rather than a reference.

It also outputs `user_ids` and `access_key_ids` keyed by identity name, so a downstream resource can write each keypair into whatever secret store the client reads from, and `buckets` listing the names it declares.

## Narrowing access without a gap

Grants are written through the admin API's upsert. Narrowing a permission set replaces it **in place** rather than revoking and re-granting, so there is no moment where the client reaches nothing:

```hcl
resource "s3orchestrator_grant" "backup" {
  user_id     = s3orchestrator_user.backup.id
  name        = "photos"
  permissions = ["read"]   # was ["list", "read", "write"]
}
```

That plans as an update, not a replacement. Changing the user, kind or name does replace the resource, because those three together are what identifies a grant - a different value addresses a different grant entirely.

Bucket permissions and administrative permissions are separate vocabularies and do not mix. A bucket grant takes `list-buckets`, `list`, `read`, `write`, `delete` and `tags`, or `all`. A backend or orchestrator grant takes the `admin-` permissions, or `admin-all`. Asking for `read` on an orchestrator grant is refused by the deployment, with a message naming what that kind does take.

## Renaming without breaking anything

A rename goes through the admin API's rename endpoint rather than being a destroy-and-create:

```hcl
resource "s3orchestrator_user" "backup" {
  name = "temporal-backup-job-v2"
}
```

The generated id does not move, so every credential and grant referencing it keeps working. A provider that replaced the user instead would revoke every keypair proving it, which is exactly what you do not want a rename to do.

## Moving a bucket out of the configuration file

A bucket the configuration file declares cannot be imported, because the API will not manage it. It can still be moved, and without taking the bucket offline to do it.

The orchestrator gives the configuration file precedence. When both sources declare the same name, the configuration entry is what serves the bucket and the stored row is set aside and reported as shadowed. That is the whole mechanism: a stored row can be put in place while the configuration entry is still there, doing nothing, waiting.

Declare it alongside the existing entry, matching what that entry carries:

```hcl
resource "s3orchestrator_bucket" "photos" {
  name = "photos"
}
```

Apply. Nothing changes for clients, and the bucket reports as shadowed:

```console
$ s3o admin bucket list
Bucket   Multipart  Source
------   ---------  ------
photos   unlimited  config

notice: bucket_shadowed: stored bucket "photos" is shadowed by a config bucket
```

Now remove the bucket from the `buckets:` block of the configuration file and reload. The row stops being shadowed and takes over:

```console
$ s3o admin bucket list
Bucket   Multipart  Source
------   ---------  ------
photos   unlimited  store
```

At no instant is the bucket undeclared - the configuration serves it until the reload, the stored row serves it afterwards. Putting the configuration entry back reverses it just as cleanly, which makes this safe to do one bucket at a time on a deployment carrying live traffic.

Two things are worth checking before removing an entry. A configuration-declared bucket carries credentials, and removing the bucket removes the identity those credentials prove, so confirm nothing still authenticates with them. And match the settings when you declare the resource: a difference in the multipart limit or the CORS rules takes effect at the handover rather than at a time you chose.

From orchestrator 0.148 a configuration file may declare no buckets at all, so the last one can leave too and the `buckets:` block can go entirely. Before that a deployment had to keep one there, whether it wanted it or not.

## Referencing a bucket you do not manage

A grant has to name a bucket that exists, and a bucket the configuration file declares is one the provider will not manage. The data source reads it either way:

```hcl
data "s3orchestrator_bucket" "photos" {
  name = "photos"
}

resource "s3orchestrator_grant" "backup" {
  user_id     = s3orchestrator_user.backup.id
  name        = data.s3orchestrator_bucket.photos.name
  permissions = ["list", "read"]
}
```

That fails the plan when the bucket does not exist, rather than failing the apply when the grant is refused. It also reports `source`, which is how a configuration tells whether a given bucket is one it may manage.

## Adopting what already exists

A deployment provisioned by hand can be brought under Terraform without recreating anything.

```bash
terraform import s3orchestrator_user.backup user-4kqx7n2mjb5tza6wfhc3
terraform import s3orchestrator_credential.backup MFRGGZDFMZTWQ2LKNNWG
terraform import s3orchestrator_grant.backup user-4kqx7n2mjb5tza6wfhc3/bucket/photos
```

A user is imported by its generated id, which `s3o admin user list` prints. A credential is imported by its access key. A grant has no identifier of its own - the orchestrator keys it by user, kind and name together - so the import identifier is those three joined by a slash. A grant on the orchestrator takes no name, so its identifier ends in an empty third segment, and the trailing slash is required:

```bash
terraform import s3orchestrator_grant.operator user-4kqx7n2mjb5tza6wfhc3/orchestrator/
```

An imported credential carries no secret, because the orchestrator never hands one back. State holds a null secret until the configuration supplies the one you already hold, which is one more reason to supply a keypair rather than mint it.

## Granting an operator the control plane

The same three resources cover an operator credential that reaches the control plane without reaching object data. A grant over the orchestrator takes no resource name, because there is only one of it:

```hcl
resource "s3orchestrator_user" "oncall" {
  name = "oncall"
}

resource "s3orchestrator_grant" "oncall" {
  user_id     = s3orchestrator_user.oncall.id
  kind        = "orchestrator"
  permissions = ["admin-read", "admin-logs"]
}
```

That identity can read status and logs and nothing else. Holding no bucket grant, it reaches no object data at all.
