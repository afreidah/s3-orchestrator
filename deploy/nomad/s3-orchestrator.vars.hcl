# -------------------------------------------------------------------------------
# S3 Orchestrator - Production Job Variables
#
# Author: Alex Freidah
#
# Production values for s3-orchestrator.nomad.hcl: two instances on separate
# nodes behind Traefik through Consul, secrets from Vault through workload
# identity. The release to run is passed at submit time with -var=version.
# Two instances need redis in config.yaml, so both enforce usage limits
# against the same counters.
# -------------------------------------------------------------------------------

# --- Placement ---
count          = 2
distinct_hosts = true

# --- Service registration and Traefik routing ---
service_provider = "consul"
service_tags = [
  "traefik.enable=true",
  "traefik.http.routers.s3-orchestrator.rule=Host(`s3.example.com`)",
  "traefik.http.routers.s3-orchestrator.entrypoints=websecure",
  "traefik.http.routers.s3-orchestrator.tls=true",
]

# --- Resources ---
resources = {
  cpu        = 1000
  memory     = 1024
  memory_max = 0
}

# --- Config, with secrets referenced as ${NAME} ---
config_file = "config.yaml"

# --- Secrets from Vault, one environment variable per config reference ---
vault_role = "s3-orchestrator"

env_template = <<EOH
{{ with secret "secret/data/s3-orchestrator" }}
DB_PASSWORD={{ .Data.data.db_password }}
REDIS_PASSWORD={{ .Data.data.redis_password }}
ROOT_ACCESS_KEY_ID={{ .Data.data.root_access_key_id }}
ROOT_SECRET_ACCESS_KEY={{ .Data.data.root_secret_access_key }}
UI_SESSION_SECRET={{ .Data.data.ui_session_secret }}
BACKEND1_ACCESS_KEY={{ .Data.data.backend1_access_key }}
BACKEND1_SECRET_KEY={{ .Data.data.backend1_secret_key }}
{{ end }}
EOH
