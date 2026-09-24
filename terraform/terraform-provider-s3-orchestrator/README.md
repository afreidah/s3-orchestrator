# Terraform Provider for S3 Orchestrator

Declares the identities a running orchestrator authorizes requests against: users, the keypairs that prove them, and the grants that say what they reach.

Buckets are not managed here. The ones a deployment serves are declared in its configuration file, which the admin API refuses to change, so a resource for them would refuse most of the buckets an operator actually has.

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
| `s3orchestrator_user` | An identity credentials prove and grants empower | The generated user id |
| `s3orchestrator_credential` | One keypair proving one user | The access key |
| `s3orchestrator_grant` | What one user reaches on one resource | `user_id/kind/name` |

These resources behave as follows when a configuration changes.

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

The Terraform Registry requires a repository named `terraform-provider-s3-orchestrator`, which this directory is not. Development happens here; publishing will mean pushing this subtree to a mirror repository of that name, which `git subtree` does without a second checkout:

```bash
git subtree push --prefix=terraform/terraform-provider-s3-orchestrator mirror main
```

The mirror holds no history of its own, so it never has to be merged back.
