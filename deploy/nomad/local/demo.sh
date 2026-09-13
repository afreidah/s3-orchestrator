#!/bin/bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Local Nomad Demo
#
# Author: Alex Freidah
#
# Stands up a complete s3-orchestrator environment using Nomad in dev mode with
# PostgreSQL and MinIO backends running via docker-compose on the host. Builds
# the image from source and submits the job. Tears down cleanly with "down".
#
# Usage:
#   ./demo.sh        # stand up the full environment
#   ./demo.sh down   # tear everything down
# -------------------------------------------------------------------------------

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE="s3-orchestrator:local"
PORT=9000

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

ADMIN_TOKEN="admin"
BUCKET="photos"
PERF_USER="perf"
PERF_GRANTS="list-buckets,list,read,write,delete"
CREDENTIALS_FILE="$SCRIPT_DIR/.perf-credentials.env"

# s3o runs the admin CLI out of the image the demo just built, so the demo needs
# no host-installed binary beyond docker.
s3o() {
    docker run --rm --network host "$IMAGE" \
        admin -addr "http://127.0.0.1:$PORT" -token "$ADMIN_TOKEN" "$@"
}

# provision_perf_identity creates the user, mints its keypair and grants it the
# bucket, writing the keypair where the perf suite reads it.
#
# Idempotent, because the demo can be re-run against a database that survived
# the last one: the user and the grant are reused where they already exist. A
# fresh keypair is minted every run regardless - a minted secret is returned
# once and never read back, so a previous run's is unrecoverable, and issuing a
# second keypair for one user is exactly what the model is for.
provision_perf_identity() {
    echo "Provisioning the '$PERF_USER' identity..."
    local listing user_id has_grant minted access_key secret

    # Every call is tolerated rather than fatal: the environment is already up by
    # this point, and losing the whole demo over a provisioning hiccup would be a
    # worse outcome than falling back to the config credential.
    listing=$(s3o -json user list 2>/dev/null || echo '{}')
    user_id=$(jq -r --arg n "$PERF_USER" \
        'first(.users[]? | select(.name == $n) | .id) // ""' <<<"$listing" 2>/dev/null || echo "")
    has_grant=$(jq -r --arg n "$PERF_USER" --arg b "$BUCKET" \
        'any(.users[]? | select(.name == $n) | .grants[]?;
             .kind == "bucket" and .name == $b)' <<<"$listing" 2>/dev/null || echo false)

    if [[ -z "$user_id" ]]; then
        user_id=$(s3o -json user create -name "$PERF_USER" 2>/dev/null \
            | jq -r '.user_id // ""' 2>/dev/null || echo "")
    fi
    if [[ -z "$user_id" ]]; then
        echo "Warning: could not provision '$PERF_USER'; the perf suite will fall"
        echo "         back to the config credential and its full access."
        return 0
    fi

    if [[ "$has_grant" != "true" ]]; then
        s3o grant add -user "$user_id" -name "$BUCKET" -permissions "$PERF_GRANTS" >/dev/null 2>&1 || true
    fi

    minted=$(s3o -json credential issue -user "$user_id" -label "perf suite" 2>/dev/null || echo '{}')
    access_key=$(jq -r '.access_key_id // ""' <<<"$minted" 2>/dev/null || echo "")
    secret=$(jq -r '.secret_access_key // ""' <<<"$minted" 2>/dev/null || echo "")
    if [[ -z "$access_key" || -z "$secret" ]]; then
        echo "Warning: could not mint a keypair for '$PERF_USER'; the perf suite"
        echo "         will fall back to the config credential."
        return 0
    fi

    # The secret reaches a file the caller owns and nothing else. Written in a
    # subshell so the tightened umask does not outlive this function.
    (
        umask 077
        cat > "$CREDENTIALS_FILE" <<EOF
# Written by demo.sh. The perf suite signs as this identity, so a run goes
# through a stored grant rather than the config credential's full access.
PERF_ACCESS_KEY="$access_key"
PERF_SECRET_KEY="$secret"
PERF_USER_ID="$user_id"
EOF
    )
    echo "  user $user_id, key $access_key, granted $PERF_GRANTS on $BUCKET"
}

# --- Teardown ---
if [[ "${1:-}" == "down" ]]; then
    echo "Tearing down demo environment..."
    nomad job stop -purge s3-orchestrator 2>/dev/null || true
    pkill -f '[n]omad agent -dev' 2>/dev/null || true
    rm -f /tmp/nomad-demo.pid "$CREDENTIALS_FILE"
    docker compose -f docker-compose.test.yml down -v 2>/dev/null || true
    echo "Done."
    exit 0
fi

# --- Preflight checks ---
for cmd in docker nomad jq; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Error: $cmd is required but not installed."
        exit 1
    fi
done

# --- Start backing services ---
echo "Starting PostgreSQL and MinIO via docker-compose..."
docker compose -f docker-compose.test.yml up -d --wait postgres minio-1 minio-2 minio-3
docker compose -f docker-compose.test.yml up -d minio-setup

# --- Start monitoring ---
echo "Starting Prometheus and Grafana..."

# --- Start Nomad dev agent ---
if nomad status &>/dev/null; then
    echo "Nomad agent already running, reusing it."
else
    echo "Starting Nomad dev agent..."
    nomad agent -dev -log-level=WARN &>/tmp/nomad-demo.log &
    echo $! > /tmp/nomad-demo.pid
    echo "Waiting for Nomad to be ready..."
    for i in $(seq 1 30); do
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

# --- Build image ---
echo "Building container image..."
docker build -t "$IMAGE" .

# --- Discover host IP ---
# In dev mode, Nomad runs Docker tasks on the host network. The Docker bridge
# gateway lets containers reach host-bound ports (docker-compose services).
HOST_IP=$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')
echo "Host gateway IP: $HOST_IP"

docker compose -f docker-compose.test.yml up -d tempo loki alloy prometheus grafana

# --- Submit job ---
echo "Submitting Nomad job..."
sed "s/__HOST_IP__/$HOST_IP/g" "$SCRIPT_DIR/s3-orchestrator.nomad.hcl" | nomad job run -detach -

# --- Wait for healthy allocation ---
echo "Waiting for allocation to become healthy..."
for i in $(seq 1 60); do
    HEALTH=$(curl -s "http://localhost:$PORT/health" 2>/dev/null || true)
    if echo "$HEALTH" | grep -q '"status":"ok"'; then
        break
    fi
    sleep 1
done

HEALTH=$(curl -s "http://localhost:$PORT/health" 2>/dev/null || true)
if echo "$HEALTH" | grep -q '"status":"ok"'; then
    # --- Provision the perf identity ---
    #
    # The config file declares one credential on "photos", and a config
    # credential carries full access because the file has no syntax for
    # narrowing it. Running the perf suite as that credential would measure the
    # request path with the permission check trivially satisfied, so the demo
    # provisions a stored user instead and grants it exactly what the suite
    # does: list, read, write, delete. Tagging is deliberately absent - the
    # suite runs no tagging scenario, and a grant that carried it would not be
    # proving anything.
    provision_perf_identity

    # --- Create Grafana trace→log correlation ---
    curl -s -X POST http://localhost:13000/api/datasources/uid/tempo/correlations \
        -H "Content-Type: application/json" \
        -d @deploy/monitoring/grafana/correlation.json >/dev/null 2>&1 || true

    echo ""
    echo "========================================"
    echo "  S3 Orchestrator is running in Nomad"
    echo "========================================"
    echo ""
    echo "  S3 API:     http://localhost:$PORT"
    echo "  Dashboard:  http://localhost:$PORT/ui/"
    echo "  Metrics:    http://localhost:9001/metrics  (dedicated listener)"
    echo "  pprof:      http://localhost:9001/debug/pprof/"
    echo "  Health:     http://localhost:$PORT/health"
    echo "  Grafana:    http://localhost:13000"
    echo "  Tempo:      http://localhost:3200"
    echo "  Nomad UI:   http://localhost:4646"
    echo ""
    echo "  Dashboard login: admin / admin"
    echo ""
    echo "  Test upload:"
    echo "    aws --endpoint-url http://localhost:$PORT s3 cp /etc/hostname s3://$BUCKET/test.txt"
    echo ""
    echo "  The '$PERF_USER' identity holds $PERF_GRANTS on $BUCKET."
    echo "  Its keypair is in $CREDENTIALS_FILE, and 'make perf' signs as it."
    echo ""
    echo "  See what it reaches:"
    echo "    s3-orchestrator admin -addr http://localhost:$PORT -token $ADMIN_TOKEN user list"
    echo ""
    echo "  Nomad agent log: /tmp/nomad-demo.log"
    echo ""
    echo "  Tear down:"
    echo "    ./deploy/nomad/local/demo.sh down"
    echo ""
else
    echo "Error: health check returned '$HEALTH' (expected 'ok')"
    nomad job status s3-orchestrator
    ALLOC_ID=$(nomad job status s3-orchestrator | grep -oP '[a-f0-9]{8}' | head -1)
    if [[ -n "$ALLOC_ID" ]]; then
        nomad alloc logs "$ALLOC_ID" | tail -20
    fi
    exit 1
fi
