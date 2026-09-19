# -----------------------------------------------------------------------------
# S3-ORCHESTRATOR Module Version Requirements
# -----------------------------------------------------------------------------

terraform {
  required_version = ">= 1.5"

  required_providers {
    # 0.147 is where s3orchestrator_bucket arrives; an older provider installs
    # cleanly and then fails on an unsupported resource type.
    s3orchestrator = {
      source  = "afreidah/s3-orchestrator"
      version = ">= 0.147"
    }
  }
}
