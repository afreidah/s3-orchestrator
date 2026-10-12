# -------------------------------------------------------------------------------
# Nomad Dev Agent - Local Demo Configuration
#
# Author: Alex Freidah
#
# Merged into "nomad agent -dev" by demo.sh. Dev mode already provides what
# most of the demo needs, Docker bind mounts included; this adds memory
# oversubscription, so each backing and observability task reserves only
# what it needs idle and bursts to its memory_max under a perf run. Without
# it a task is capped at its reservation, and reserving the burst instead
# would leave too little memory on a laptop to place the orchestrator fleet.
# -------------------------------------------------------------------------------

server {
  default_scheduler_config {
    memory_oversubscription_enabled = true
  }
}
