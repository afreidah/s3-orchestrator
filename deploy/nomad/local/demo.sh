#!/bin/bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Local Nomad Demo
#
# Author: Alex Freidah
#
# Stands up a complete s3-orchestrator environment using Nomad in dev mode with
# PostgreSQL, Redis and MinIO backends running via docker-compose on the host.
# Builds the image from source and runs a fleet of instances behind Traefik,
# the way production runs them. Everything but the scheduler is shared with the
# Kubernetes demo through deploy/local. Tears down cleanly with "down".
#
# Usage:
#   ./demo.sh                # stand up the full environment
#   INSTANCES=1 ./demo.sh    # a single instance, still behind Traefik
#   ./demo.sh down           # tear everything down
# -------------------------------------------------------------------------------

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=SCRIPTDIR/../../local/lib.sh
source "$SCRIPT_DIR/../../local/lib.sh"

# Pin every Nomad AND Consul endpoint/credential to the local dev agent so a
# sourced prod profile (e.g. munchbox-env.sh) can never redirect us at a real
# cluster. The Nomad dev agent's Consul integration falls back to CONSUL_HTTP_*
# env vars, so leaving those set makes the dev agent self-register nomad/
# nomad-client services into the real Consul -- clear them too. The demo uses
# Nomad-native service discovery (provider="nomad"), so no Consul is needed.
unset NOMAD_TOKEN NOMAD_CACERT NOMAD_CLIENT_CERT NOMAD_CLIENT_KEY NOMAD_TLS_SERVER_NAME NOMAD_NAMESPACE NOMAD_REGION
unset CONSUL_HTTP_TOKEN CONSUL_CACERT CONSUL_CLIENT_CERT CONSUL_CLIENT_KEY CONSUL_TLS_SERVER_NAME CONSUL_HTTP_SSL CONSUL_HTTP_SSL_VERIFY CONSUL_NAMESPACE
export NOMAD_ADDR="http://127.0.0.1:4646"
export CONSUL_HTTP_ADDR="http://127.0.0.1:8500"
nomad() { NOMAD_ADDR="http://127.0.0.1:4646" command nomad "$@"; }

cd "$REPO_ROOT"

# render_job prints the orchestrator job with the rendered config inlined at
# the __CONFIG__ line, indented to match it, and the instance count filled in.
render_job() {
    local config="$1"
    awk -v cfg="$config" -v n="$INSTANCES" '
        /^[ \t]*__CONFIG__[ \t]*$/ {
            indent = substr($0, 1, index($0, "__CONFIG__") - 1)
            while ((getline line < cfg) > 0) print (line == "" ? "" : indent line)
            next
        }
        { gsub(/__INSTANCES__/, n); print }
    ' "$SCRIPT_DIR/s3-orchestrator.nomad.hcl"
}

# healthy_allocations counts the orchestrator allocations that are running and
# have passed their checks.
healthy_allocations() {
    curl -s "$NOMAD_ADDR/v1/job/s3-orchestrator/allocations" 2>/dev/null \
        | jq '[.[] | select(.ClientStatus == "running" and .DeploymentStatus.Healthy == true)] | length' 2>/dev/null \
        || echo 0
}

# print_platform_endpoints lists what only Nomad can report: the dev agent UI
# and each instance's dynamic metrics port.
print_platform_endpoints() {
    echo "  Nomad UI:   http://localhost:4646"
    echo "  Metrics and pprof, one listener per instance:"
    nomad service info -json s3-orchestrator-metrics 2>/dev/null \
        | jq -r '.[] | "    http://\(.Address):\(.Port)/metrics  (alloc \(.AllocID[0:8]))"' 2>/dev/null || true
}

# --- Teardown ---
if [[ "${1:-}" == "down" ]]; then
    echo "Tearing down demo environment..."
    nomad job stop -purge s3-orchestrator 2>/dev/null || true
    nomad job stop -purge traefik 2>/dev/null || true
    pkill -f '[n]omad agent -dev' 2>/dev/null || true
    rm -f /tmp/nomad-demo.pid "$CREDENTIALS_FILE"
    stop_backing_services
    echo "Done."
    exit 0
fi

require_commands docker nomad jq
start_backing_services

# --- Start Nomad dev agent ---
if nomad status &>/dev/null; then
    echo "Nomad agent already running, reusing it."
else
    echo "Starting Nomad dev agent..."
    nomad agent -dev -log-level=WARN &>/tmp/nomad-demo.log &
    echo $! > /tmp/nomad-demo.pid
    echo "Waiting for Nomad to be ready..."
    for _ in $(seq 1 30); do
        if nomad status &>/dev/null; then
            break
        fi
        sleep 1
    done
    if ! nomad status &>/dev/null; then
        echo "Error: Nomad agent failed to start. Check /tmp/nomad-demo.log"
        exit 1
    fi
fi

build_image

# --- Discover host IP ---
# In dev mode, Nomad runs Docker tasks on the default bridge. Its gateway lets
# containers reach host-bound ports (docker-compose services).
HOST_IP=$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')
echo "Host gateway IP: $HOST_IP"

# Alloy runs in compose here, tailing container logs through the docker socket.
docker compose -f "$COMPOSE_FILE" up -d tempo loki alloy prometheus grafana

# --- Submit jobs ---
echo "Submitting Traefik job..."
nomad job run -detach "$SCRIPT_DIR/traefik.nomad.hcl"

echo "Submitting s3-orchestrator job ($INSTANCES instances)..."
RENDERED_CONFIG="$(mktemp)"
trap 'rm -f "$RENDERED_CONFIG"' EXIT
render_config "$HOST_IP" "$RENDERED_CONFIG"
render_job "$RENDERED_CONFIG" | nomad job run -detach -

# --- Wait for a healthy fleet ---
# Every instance must pass its checks before traffic starts: the perf suite
# measures the whole fleet, and a run that began against one instance while
# the others were still booting would measure something else.
echo "Waiting for $INSTANCES healthy allocations..."
HEALTHY=0
for _ in $(seq 1 120); do
    HEALTHY=$(healthy_allocations)
    if [[ "$HEALTHY" -ge "$INSTANCES" ]]; then
        break
    fi
    sleep 1
done

# Traefik picks up new registrations on its next refresh.
echo "Waiting for Traefik to route to the fleet..."
if [[ "$HEALTHY" -ge "$INSTANCES" ]] && wait_for_health 30; then
    provision_perf_identity
    create_grafana_correlation
    print_summary "Nomad" "./deploy/nomad/local/demo.sh"
    echo "  Nomad agent log: /tmp/nomad-demo.log"
    echo ""
else
    echo "Error: $HEALTHY of $INSTANCES allocations healthy, or Traefik is not routing to them"
    nomad job status s3-orchestrator
    ALLOC_ID=$(nomad job status s3-orchestrator | grep -oP '[a-f0-9]{8}' | head -1)
    if [[ -n "$ALLOC_ID" ]]; then
        nomad alloc logs "$ALLOC_ID" | tail -20
    fi
    exit 1
fi
