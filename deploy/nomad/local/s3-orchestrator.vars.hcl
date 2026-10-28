# -------------------------------------------------------------------------------
# S3 Orchestrator - Local Dev Pack Variables (nomad agent -dev)
#
# Author: Alex Freidah
#
# Only what the Nomad demo needs of the s3_orchestrator pack. demo.sh passes the
# rendered deploy/local/config.yaml as config_file and the instance count. The
# shared config carries its dev credentials inline, so the task needs no
# environment. Not for production use.
# -------------------------------------------------------------------------------

# --- The image demo.sh builds ---
image   = "s3-orchestrator"
version = "local"

# --- Routed by the demo's Traefik job on a catch-all rule ---
service_tags = [
  "traefik.enable=true",
  "traefik.http.routers.s3-orchestrator.entrypoints=s3",
  "traefik.http.routers.s3-orchestrator.rule=PathPrefix(`/`)",
]

# --- The same limits as the Kubernetes demo's pods ---
resources = {
  cpu        = 2000
  memory     = 1024
  memory_max = 0
}

# --- Credentials are inline in the shared config ---
env_template = ""

# --- A moved backing service restarts the fleet once, onto its new address ---
config_wait = {
  min = "30s"
  max = "2m"
}
