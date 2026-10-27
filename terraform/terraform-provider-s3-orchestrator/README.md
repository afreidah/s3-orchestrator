# Terraform Provider for S3 Orchestrator

Declares the virtual buckets a running orchestrator serves and the identities it authorizes requests against: users, the keypairs that prove them, and the grants that say what they reach.

Published as `afreidah/s3-orchestrator` to the [Terraform Registry](https://registry.terraform.io/providers/afreidah/s3-orchestrator/latest/docs) and the [OpenTofu Registry](https://search.opentofu.org/provider/afreidah/s3-orchestrator/latest).

Buckets, users and keypairs declared in the orchestrator's configuration file stay read-only to the admin API, so the provider cannot manage them. The `s3orchestrator_bucket` data source reads a config-declared bucket so a grant can name it.

## Quick Start

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
  address = "http://localhost:9000"
}

resource "s3orchestrator_bucket" "photos" {
  name = "photos"
}

resource "s3orchestrator_user" "backup" {
  name = "temporal-backup-job"
}

resource "s3orchestrator_credential" "backup" {
  user_id = s3orchestrator_user.backup.id
  label   = "vault-managed"
}

resource "s3orchestrator_grant" "backup" {
  user_id     = s3orchestrator_user.backup.id
  name        = s3orchestrator_bucket.photos.name
  permissions = ["list", "read", "write"]
}
```

For onboarding several clients at once, use the module in `terraform/modules/s3-orchestrator/` instead: it takes a map of identities and fans the three resources out over it.

## Configuration

Every provider attribute falls back to an environment variable, the same three the admin CLI and the TUI read, so a shell configured for one is configured for all of them.

| Attribute | Environment variable | Description |
|-----------|----------------------|-------------|
| `address` | `S3O_ADMIN_ADDR` | Base URL of the orchestrator's admin API |
| `access_key_id` | `S3O_ACCESS_KEY_ID` | Access key requests are signed with |
| `secret_access_key` | `S3O_SECRET_ACCESS_KEY` | Secret half of that keypair |

Requests are signed with SigV4, so the credential needs the admin permissions for what it is asked to do. `admin-provision` covers every resource here.

## Resources

| Resource | Manages | Import identifier |
|----------|---------|-------------------|
| `s3orchestrator_bucket` | A virtual bucket, its CORS rules and its multipart upload limit | The bucket name |
| `s3orchestrator_user` | An identity credentials prove and grants empower | The generated user id |
| `s3orchestrator_credential` | One keypair proving one user | The access key |
| `s3orchestrator_grant` | What one user reaches on one resource | `user_id/kind/name` |

The `s3orchestrator_bucket` data source reads an existing bucket from either source, and reports which one declared it as `config` or `store`.

These resources behave as follows when a configuration changes.

A **bucket** declares a namespace on the orchestrator; it creates nothing on any backend. Renaming it replaces it, and everything else updates in place. Dropping a `cors_rule` block removes that rule. The orchestrator refuses to delete a bucket while it holds an object or a grant names it. A bucket also declared in the configuration file is served from there, and the one created here takes over once that entry is removed and the configuration reloaded, so a bucket moves into the store with no gap.

A **user rename** is an update rather than a replacement. The id does not move, so the credentials and grants referencing it keep working.

A **credential** takes both halves of a keypair or neither. Supplying both registers one generated elsewhere, which is how this composes with a secret manager. Supplying neither mints one, and the minted secret crosses the wire exactly once and lives in Terraform state afterwards, so losing state loses the credential. An import cannot carry the secret, because the orchestrator never reads one back out.

A **grant** is upserted, so narrowing a permission set replaces it in place rather than revoking and re-granting. There is no window where the client reaches nothing. Changing the user, kind or name addresses a different grant and does replace the resource. A grant naming no `kind` is over a bucket.

Bucket and administrative permissions are separate vocabularies and do not mix. Buckets take `list-buckets`, `list`, `read`, `write`, `delete` and `tags`, or `all`. Backends and the orchestrator take the `admin-` permissions, or `admin-all`.

See `examples/` for each resource on its own, including what an import looks like.

## Developing

The provider is its own Go module, so the repository-root `./...` never reaches it. Build it in place:

```bash
cd terraform/terraform-provider-s3-orchestrator
go build .
```

To run a configuration against that build rather than a released one, point Terraform at the directory holding the binary:

```hcl
# ~/.terraformrc, or a file named by TF_CLI_CONFIG_FILE
provider_installation {
  dev_overrides {
    "afreidah/s3-orchestrator" = "/path/to/s3-orchestrator/terraform/terraform-provider-s3-orchestrator"
  }
  direct {}
}
```

With an override in place `terraform init` is skipped for this provider, and `terraform plan` prints a warning saying so. That warning is the confirmation the override took.

### Tests

Acceptance tests drive real `terraform plan`, `apply`, `refresh` and `import` against an orchestrator running in a container, started by testcontainers. From the repository root:

```bash
make provider-test
```

That target builds the orchestrator image first, because the Dockerfile uses BuildKit's platform arguments and the Docker client library testcontainers builds through does not provide them. Set `S3O_TEST_IMAGE` to run an image already built instead.

Nothing starts unless `TF_ACC` is set, so a plain `go test ./...` in this directory stays fast and needs no Docker.

## Publishing

The registries require a repository named `terraform-provider-s3-orchestrator`, which this directory is not. Development happens here, and [afreidah/terraform-provider-s3-orchestrator](https://github.com/afreidah/terraform-provider-s3-orchestrator) mirrors it. From the repository root:

```bash
make provider-publish
```

That regenerates the registry docs and stops if they changed, pushes this subtree to the mirror's `main` with `git subtree`, and tags the mirror with the version in `.version`. The mirror's release workflow builds and signs that tag, and both registries pick up the release. The mirror holds no history of its own, so it never has to be merged back.
