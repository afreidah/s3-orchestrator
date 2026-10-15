#!/bin/bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Local Nomad Demo
#
# Author: Alex Freidah
#
# Stands up a complete s3-orchestrator environment using Nomad in dev mode,
# with every backing service running as a Nomad job: PostgreSQL, Redis and
# MinIO in services.nomad.hcl, the observability stack in
# observability.nomad.hcl. Builds the image from source and runs a fleet of
# instances behind Traefik, the way production runs them. The instances find
# PostgreSQL, Redis and MinIO through Nomad's service registry. Everything but
# the scheduler is shared with the Kubernetes demo through deploy/local. Tears
# down cleanly with "down".
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

# Every job the demo submits, in the order teardown stops them.
DEMO_JOBS=(s3-orchestrator traefik demo-minio-setup demo-observability demo-services)

# The named volumes services.nomad.hcl keeps PostgreSQL and MinIO data on.
# Nomad never removes a named volume, so teardown does.
DEMO_VOLUMES=(demo-minio-1-data demo-minio-2-data demo-minio-3-data demo-postgres-data)

# Nomad's Docker driver labels every container it starts with this, which is
# how teardown finds the ones still running.
NOMAD_CONTAINER_LABEL="com.hashicorp.nomad.alloc_id"

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

# healthy_allocations counts the job's allocations that are running and have
# passed their checks.
healthy_allocations() {
    local job="$1"
    curl -s "$NOMAD_ADDR/v1/job/$job/allocations" 2>/dev/null \
        | jq '[.[] | select(.ClientStatus == "running" and .DeploymentStatus.Healthy == true)] | length' 2>/dev/null \
        || echo 0
}

# wait_for_job waits up to the given number of seconds for every allocation
# the job asks for - the sum of its groups' counts - to be running and
# healthy, and shows the job's status if they are not.
wait_for_job() {
    local job="$1" seconds="$2" want healthy=0 _
    want=$(curl -s "$NOMAD_ADDR/v1/job/$job" 2>/dev/null \
        | jq '[.TaskGroups[].Count] | add' 2>/dev/null || echo 1)
    for _ in $(seq 1 "$seconds"); do
        healthy=$(healthy_allocations "$job")
        if [[ "$healthy" -ge "$want" ]]; then
            return 0
        fi
        sleep 1
    done
    echo "Error: $healthy of $want $job allocations healthy after ${seconds}s"
    nomad job status "$job" || true
    return 1
}

# wait_for_postgres waits until PostgreSQL accepts connections. Its Nomad
# check reaches it through Docker's userland proxy, which can accept a
# connection before Postgres listens, so a passing check is not enough.
wait_for_postgres() {
    local alloc_id _
    alloc_id=$(curl -s "$NOMAD_ADDR/v1/job/demo-services/allocations" 2>/dev/null \
        | jq -r '[.[] | select(.TaskGroup == "postgres" and .ClientStatus == "running")][0].ID // ""' \
            2>/dev/null || echo "")
    if [[ -n "$alloc_id" ]]; then
        for _ in $(seq 1 60); do
            if nomad alloc exec -i=false -t=false -task postgres "$alloc_id" \
                pg_isready -h 127.0.0.1 -U s3proxy -d s3proxy_test &>/dev/null; then
                return 0
            fi
            sleep 1
        done
    fi
    echo "Error: PostgreSQL is not accepting connections"
    nomad job status demo-services || true
    exit 1
}

# run_minio_setup creates the backend buckets with the one-shot batch job and
# waits for it to finish. An earlier run is purged first: a batch job that
# already completed is not run again when resubmitted unchanged, and the
# backends it set up may since have been replaced.
run_minio_setup() {
    local summary complete=0 failed=0 _
    echo "Creating the MinIO backend buckets..."
    nomad job stop -purge demo-minio-setup &>/dev/null || true
    nomad job run -detach "$SCRIPT_DIR/minio-setup.nomad.hcl" >/dev/null
    for _ in $(seq 1 180); do
        summary=$(curl -s "$NOMAD_ADDR/v1/job/demo-minio-setup/summary" 2>/dev/null || echo '{}')
        complete=$(jq '.Summary["minio-setup"].Complete // 0' <<<"$summary" 2>/dev/null || echo 0)
        failed=$(jq '.Summary["minio-setup"].Failed // 0' <<<"$summary" 2>/dev/null || echo 0)
        if [[ "$complete" -ge 1 || "$failed" -ge 1 ]]; then
            break
        fi
        sleep 1
    done
    if [[ "$complete" -lt 1 ]]; then
        echo "Error: the MinIO bucket setup job did not complete"
        nomad job status demo-minio-setup || true
        exit 1
    fi
}

# check_reused_agent stops the demo when an agent it did not start cannot run
# its jobs, and warns about what will merely behave differently. The
# observability tasks bind-mount their configs and Alloy the Docker socket,
# and PostgreSQL and MinIO mount named volumes, all of which need the Docker
# volumes agent.hcl enables; and without the memory oversubscription it also
# enables, every task is capped at its reservation rather than its
# memory_max.
check_reused_agent() {
    local node_id volumes oversubscription
    node_id=$(curl -s "$NOMAD_ADDR/v1/nodes" 2>/dev/null | jq -r '.[0].ID // ""' 2>/dev/null || echo "")
    volumes=$(curl -s "$NOMAD_ADDR/v1/node/$node_id" 2>/dev/null \
        | jq -r '.Attributes["driver.docker.volumes.enabled"] // "false"' 2>/dev/null || echo false)
    if [[ "$volumes" != "true" ]]; then
        echo "Error: the running Nomad agent does not enable Docker volumes, which the"
        echo "       demo's jobs need. Stop it ('$SCRIPT_DIR/demo.sh down') and re-run"
        echo "       the demo, which starts its own dev agent with agent.hcl."
        exit 1
    fi
    oversubscription=$(curl -s "$NOMAD_ADDR/v1/operator/scheduler/configuration" 2>/dev/null \
        | jq -r '.SchedulerConfig.MemoryOversubscriptionEnabled // false' 2>/dev/null || echo false)
    if [[ "$oversubscription" != "true" ]]; then
        echo "Warning: memory oversubscription is off on the running agent, so each"
        echo "         backing service is capped at its reserved memory. Start the"
        echo "         agent with -config=$SCRIPT_DIR/agent.hcl to lift the caps."
    fi
}

# remove_leftovers removes what outlives the agent: containers it did not get
# to stop, its log shipper processes, and the data volumes, which Nomad never
# removes. Run once the agent has exited; every step tolerates failure.
remove_leftovers() {
    { docker ps -aq --filter "label=$NOMAD_CONTAINER_LABEL" 2>/dev/null || true; } \
        | while read -r container; do
            docker rm -f "$container" >/dev/null 2>&1 || true
        done
    pkill -f '[n]omad (logmon|docker_logger)' 2>/dev/null || true
    docker volume rm -f "${DEMO_VOLUMES[@]}" >/dev/null 2>&1 || true
    rm -f /tmp/nomad-demo.pid "$CREDENTIALS_FILE"
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
# "job stop" returns once the stop is scheduled, not once the containers are
# gone. So teardown waits for Nomad's containers to exit before stopping the
# agent, then waits for the agent to exit, so an immediate re-run cannot find
# the dying agent and reuse it, and then removes the leftovers. Every step
# tolerates failure, so teardown always runs to the end, including with
# nothing running. "make clean" runs this too.
if [[ "${1:-}" == "down" ]]; then
    echo "Tearing down demo environment..."
    for job in "${DEMO_JOBS[@]}"; do
        nomad job stop -purge "$job" 2>/dev/null || true
    done
    for _ in $(seq 1 30); do
        if [[ -z "$(docker ps -q --filter "label=$NOMAD_CONTAINER_LABEL" 2>/dev/null)" ]]; then
            break
        fi
        sleep 1
    done
    pkill -f '[n]omad agent -dev' 2>/dev/null || true
    for _ in $(seq 1 60); do
        if ! pgrep -f '[n]omad agent -dev' >/dev/null 2>&1; then
            break
        fi
        sleep 1
    done
    remove_leftovers
    echo "Done."
    exit 0
fi

require_commands docker nomad jq curl
refuse_running_dev_deps

# --- Start Nomad dev agent ---
if nomad status &>/dev/null; then
    echo "Nomad agent already running, reusing it."
    check_reused_agent
else
    echo "Starting Nomad dev agent..."
    nomad agent -dev -config="$SCRIPT_DIR/agent.hcl" -log-level=WARN &>/tmp/nomad-demo.log &
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

# --- Discover host IP ---
# In dev mode, Nomad runs Docker tasks on the default bridge. Its gateway lets
# containers reach the observability tasks that run on the host network.
HOST_IP=$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')
echo "Host gateway IP: $HOST_IP"

# --- Submit backing services ---
# Submitted before the image build so their images pull while it runs.
echo "Submitting PostgreSQL, Redis and MinIO..."
nomad job run -detach "$SCRIPT_DIR/services.nomad.hcl" >/dev/null
echo "Submitting Prometheus, Grafana, Tempo, Loki and Alloy..."
nomad job run -detach -var "host_ip=$HOST_IP" -var "repo_root=$REPO_ROOT" \
    "$SCRIPT_DIR/observability.nomad.hcl" >/dev/null

build_image

echo "Waiting for PostgreSQL, Redis and MinIO..."
wait_for_job demo-services 300 || exit 1
wait_for_postgres
run_minio_setup

# --- Submit jobs ---
echo "Submitting Traefik job..."
nomad job run -detach "$SCRIPT_DIR/traefik.nomad.hcl"

echo "Submitting s3-orchestrator job ($INSTANCES instances)..."
RENDERED_CONFIG="$(mktemp)"
trap 'rm -f "$RENDERED_CONFIG"' EXIT
# The backing-service addresses are registry lookups the job's template
# resolves at task start. Tempo is reached through the bridge gateway, since
# it runs on the host network.
lookup() { printf '{{ range nomadService "%s" }}%s{{ end }}' "$1" "$2"; }
ADDR_PORT='{{ .Address }}:{{ .Port }}'
render_config "$RENDERED_CONFIG" \
    "POSTGRES_HOST=$(lookup demo-postgres '{{ .Address }}')" \
    "POSTGRES_PORT=$(lookup demo-postgres '{{ .Port }}')" \
    "REDIS_ADDR=$(lookup demo-redis "$ADDR_PORT")" \
    "MINIO_1_ADDR=$(lookup demo-minio-1 "$ADDR_PORT")" \
    "MINIO_2_ADDR=$(lookup demo-minio-2 "$ADDR_PORT")" \
    "MINIO_3_ADDR=$(lookup demo-minio-3 "$ADDR_PORT")" \
    "OTLP_ADDR=$HOST_IP:4317"
render_job "$RENDERED_CONFIG" | nomad job run -detach -

# --- Wait for a healthy fleet ---
# Every instance must pass its checks before traffic starts: the perf suite
# measures the whole fleet, and a run that began against one instance while
# the others were still booting would measure something else.
echo "Waiting for $INSTANCES healthy allocations..."
HEALTHY=0
for _ in $(seq 1 120); do
    HEALTHY=$(healthy_allocations s3-orchestrator)
    if [[ "$HEALTHY" -ge "$INSTANCES" ]]; then
        break
    fi
    sleep 1
done

# Traefik picks up new registrations on its next refresh.
echo "Waiting for Traefik to route to the fleet..."
if [[ "$HEALTHY" -ge "$INSTANCES" ]] && wait_for_health 30; then
    provision_perf_identity
    # The correlation is created through Grafana's API, so the stack has to
    # be up first. It has had the whole fleet rollout to start, so this is
    # normally already true.
    wait_for_job demo-observability 120 || echo "Warning: continuing without a healthy observability stack."
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
