# -------------------------------------------------------------------------------
# Nomad Dev Agent - Local Demo Configuration
#
# Author: Alex Freidah
#
# Merged into "nomad agent -dev" by demo.sh, for two things the demo jobs
# need that dev mode does not provide:
#
# - Docker volumes. The observability tasks bind-mount their configs from
#   the repository and Alloy the Docker socket, and PostgreSQL and MinIO keep
#   their data on named volumes. The Docker driver refuses all of these
#   unless volumes are enabled, and dev mode leaves them off.
# - Memory oversubscription, so each backing and observability task reserves
#   only what it needs idle and bursts to its memory_max under a perf run.
#   Without it a task is capped at its reservation, and reserving the burst
#   instead would leave too little memory on a laptop to place the
#   orchestrator fleet.
# -------------------------------------------------------------------------------

plugin "docker" {
  config {
    volumes {
      enabled = true
    }
  }
}

server {
  default_scheduler_config {
    memory_oversubscription_enabled = true
  }
}
