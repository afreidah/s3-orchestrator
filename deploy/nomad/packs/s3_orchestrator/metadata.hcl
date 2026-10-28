# -------------------------------------------------------------------------------
# s3-orchestrator Pack - Metadata
#
# Author: Alex Freidah
#
# Names the pack and the application it deploys. The pack version tracks the
# binary's release: the release workflow stamps it, and the default of the
# version variable, when it packages the pack.
# -------------------------------------------------------------------------------

app {
  url = "https://s3-orchestrator.munchbox.cc"
}

pack {
  name        = "s3_orchestrator"
  description = "Unified S3-compatible storage across multiple backends, with quotas, replication and usage limits."
  version     = "0.0.0-dev"
}
