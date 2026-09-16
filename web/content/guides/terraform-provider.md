---
title: "Provisioning with Terraform"
description: "Declare the users, keypairs and grants a deployment serves, onboard a client from one map entry, narrow access without a gap, and adopt identities that already exist."
weight: 4
---


The [access control guide](../access-control/) onboards a client by hand, one admin command at a time. This one does the same work declaratively: a Terraform provider that manages the identities a running S3 Orchestrator authorizes requests against, so the access a deployment grants is a file in version control rather than a sequence of commands somebody remembers running.

## What it manages, and what it does not

The provider manages exactly what the admin API manages: **users**, the **credentials** that prove them, and the **grants** that say what they reach.

Buckets are deliberately absent. The buckets a deployment serves are declared in its configuration file, and the admin API refuses to change those, so a bucket resource would fail on most of the buckets an operator actually has. The same applies to any user or credential the configuration file declares - `s3o admin user list` shows those with a `config` source. They are visible through the API and never writable through it, so the provider refuses to import or manage one and says why, rather than letting the failure read as a credential problem.

## Installing the provider

The provider is not published to the Terraform Registry yet, so it is consumed through a development override. Build it from the repository:

```bash
cd terraform/terraform-provider-s3-orchestrator
go build .
```

Then point Terraform at the directory holding the binary, in `~/.terraformrc` or a file named by `TF_CLI_CONFIG_FILE`:

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
terraform {
  required_providers {
    s3orchestrator = {
      source  = "afreidah/s3-orchestrator"
      version = ">= 0.1"
    }
  }
}

provider "s3orchestrator" {
  address = "https://s3.example.com"
}
```

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

Adding a client becomes one map entry rather than a deploy. The module also outputs `user_ids` and `access_key_ids` keyed by identity name, so a downstream resource can write each keypair into whatever secret store the client reads from.

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
