#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# run-banner.sh - print how to reach the instance `make run` starts
#
# Waits for the instance to answer on /health, then prints the dashboard URL
# and the root keypair from the config, so the login lands below the startup
# logs rather than scrolling away above them. The keypair is read from the
# config instead of repeated here, so the two cannot drift. Gives up quietly
# if the instance never comes up; the server's own output says why.
# ---------------------------------------------------------------------------
set -euo pipefail

config="${1:-config.yaml}"

# root_value prints one field of the auth.root keypair from the config.
root_value() {
	local field="$1"
	awk -v field="$field:" '/^auth:/ { inauth = 1 } inauth && $1 == field { gsub(/"/, "", $2); print $2; exit }' "$config"
}

port=$(awk '/listen_addr:/ { gsub(/"/, "", $2); n = split($2, p, ":"); print p[n]; exit }' "$config")
addr="http://localhost:${port:-9000}"
key=$(root_value access_key_id)
secret=$(root_value secret_access_key)

for _ in $(seq 120); do
	curl -fs "$addr/health" >/dev/null 2>&1 && break
	sleep 1
done
curl -fs "$addr/health" >/dev/null 2>&1 || exit 0

cat <<EOF

========================================
  S3 Orchestrator is running
========================================

  S3 API:     $addr
  Dashboard:  $addr/ui/

  Root keypair - for the dashboard login, the TUI, and the admin API:
    access key: $key
    secret key: $secret

    export S3O_ADMIN_ADDR=$addr
    export S3O_ACCESS_KEY_ID=$key
    export S3O_SECRET_ACCESS_KEY=$secret
    go run ./cmd/s3-orchestrator tui

EOF
