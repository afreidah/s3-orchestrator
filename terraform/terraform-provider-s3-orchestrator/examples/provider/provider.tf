terraform {
  required_providers {
    s3orchestrator = {
      source  = "afreidah/s3-orchestrator"
      version = ">= 0.1"
    }
  }
}

# Every attribute falls back to an environment variable: S3O_ADMIN_ADDR,
# S3O_ACCESS_KEY_ID and S3O_SECRET_ACCESS_KEY, the same three the admin CLI and
# the TUI read. A shell configured for one is configured for all of them, so the
# block can be left empty.
#
# Naming the address here instead pins the configuration to one deployment,
# which is worth doing for anything that must never reach production by
# accident. The keypair stays in the environment either way, because a literal
# secret in a .tf file is a secret in version control.
provider "s3orchestrator" {
  address = "http://localhost:9000"
}
