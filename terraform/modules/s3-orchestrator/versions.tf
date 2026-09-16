# -----------------------------------------------------------------------------
# S3-ORCHESTRATOR Module Version Requirements
# -----------------------------------------------------------------------------

terraform {
  required_version = ">= 1.5"

  required_providers {
    s3orchestrator = {
      source  = "afreidah/s3-orchestrator"
      version = ">= 0.1"
    }
  }
}
