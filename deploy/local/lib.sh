#!/bin/bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Local Demo Helpers
#
# Author: Alex Freidah
#
# Sourced by the Nomad and Kubernetes demo scripts so both stand up the same
# environment: the same backing services, the same rendered config, the same
# minted root keypair and perf identity, and the same fleet of instances behind
# Traefik on :9000. Each demo supplies only what its scheduler does differently.
# -------------------------------------------------------------------------------

LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$LIB_DIR/../.." && pwd)"

IMAGE="s3-orchestrator:local"
PORT=9000
TRAEFIK_DASHBOARD_PORT=8081
INSTANCES="${INSTANCES:-3}"
COMPOSE_FILE="$REPO_ROOT/docker-compose.test.yml"
CONFIG_TEMPLATE="$LIB_DIR/config.yaml"

BUCKET="photos"
PERF_USER="perf"
PERF_GRANTS="list-buckets,list,read,write,delete"
CREDENTIALS_FILE="$LIB_DIR/.perf-credentials.env"

# rand_chars draws n characters from the given set.
#
# The random source is a fixed-size read rather than a stream, because a reader
# that stops early leaves the filter writing into a closed pipe: under
# "set -o pipefail" that SIGPIPE fails the whole script, which is a confusing
# way for a demo to die before it prints anything.
rand_chars() {
    local set="$1" n="$2"
    head -c 1024 /dev/urandom | LC_ALL=C tr -dc "$set" | cut -c1-"$n"
}

# The root keypair, minted per run and substituted into the config. This is the
# credential the demo administers itself with: the admin API, the TUI and the
# dashboard all take it, so there is one thing to hold rather than a token for
# one surface and a password for another.
ROOT_ACCESS_KEY="AKIA$(rand_chars 'A-Z0-9' 16)"
ROOT_SECRET_KEY="$(rand_chars 'A-Za-z0-9' 40)"

# s3o runs the admin CLI out of the image the demo just built, so the demo needs
# no host-installed binary beyond docker. It signs, rather than presenting a
# token, which is the path an operator should be on.
s3o() {
    docker run --rm --network host "$IMAGE" \
        admin -addr "http://127.0.0.1:$PORT" \
        -access-key "$ROOT_ACCESS_KEY" -secret-key "$ROOT_SECRET_KEY" "$@"
}

# require_commands exits unless every named command is installed.
require_commands() {
    local cmd
    for cmd in "$@"; do
        if ! command -v "$cmd" &>/dev/null; then
            echo "Error: $cmd is required but not installed."
            exit 1
        fi
    done
}

# start_backing_services brings up Postgres, Redis and the MinIO backends, and
# creates the backend buckets.
start_backing_services() {
    echo "Starting PostgreSQL, Redis and MinIO via docker-compose..."
    docker compose -f "$COMPOSE_FILE" up -d --wait postgres redis minio-1 minio-2 minio-3
    docker compose -f "$COMPOSE_FILE" up -d minio-setup
}

# stop_backing_services removes every compose service and its volumes.
stop_backing_services() {
    docker compose -f "$COMPOSE_FILE" down -v 2>/dev/null || true
}

# build_image builds the orchestrator image from the working tree.
build_image() {
    echo "Building container image..."
    docker build -t "$IMAGE" "$REPO_ROOT"
}

# render_config writes the shared config to dest with the placeholders filled:
# host_ip is the address the instances reach the compose services at.
render_config() {
    local host_ip="$1" dest="$2"
    sed -e "s/__HOST_IP__/$host_ip/g" \
        -e "s|__ROOT_ACCESS_KEY__|$ROOT_ACCESS_KEY|g" \
        -e "s|__ROOT_SECRET_KEY__|$ROOT_SECRET_KEY|g" \
        "$CONFIG_TEMPLATE" > "$dest"
}

# wait_for_health polls /health through Traefik until it reports ok or the
# given number of seconds passes.
wait_for_health() {
    local seconds="$1" _
    for _ in $(seq 1 "$seconds"); do
        if curl -s "http://localhost:$PORT/health" 2>/dev/null | grep -q '"status":"ok"'; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# provision_perf_identity creates the user, mints its keypair and grants it the
# bucket, writing the keypair where the perf suite reads it.
#
# The config file declares one credential on "photos", and a config credential
# carries full access because the file has no syntax for narrowing it. Running
# the perf suite as that credential would measure the request path with the
# permission check trivially satisfied, so the demo provisions a stored user
# instead and grants it exactly what the suite does. Tagging is deliberately
# absent - the suite runs no tagging scenario.
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
# Written by the local demo. The perf suite signs as the perf identity, so a run
# goes through a stored grant rather than the config credential's full access.
# The root keypair is here too, for reaching the admin API by hand.
PERF_ACCESS_KEY="$access_key"
PERF_SECRET_KEY="$secret"
PERF_USER_ID="$user_id"
S3O_ACCESS_KEY_ID="$ROOT_ACCESS_KEY"
S3O_SECRET_ACCESS_KEY="$ROOT_SECRET_KEY"
EOF
    )
    echo "  user $user_id, key $access_key, granted $PERF_GRANTS on $BUCKET"
}

# create_grafana_correlation links Tempo traces to Loki logs in Grafana.
create_grafana_correlation() {
    curl -s -X POST http://localhost:13000/api/datasources/uid/tempo/correlations \
        -H "Content-Type: application/json" \
        -d @"$REPO_ROOT/deploy/monitoring/grafana/correlation.json" >/dev/null 2>&1 || true
}

# print_summary prints how to reach the environment. The caller passes its
# scheduler's name, and defines print_platform_endpoints for the lines only it
# can fill in.
print_summary() {
    local platform="$1" demo_script="$2"
    echo ""
    echo "========================================"
    echo "  S3 Orchestrator is running in $platform"
    echo "========================================"
    echo ""
    echo "  S3 API:     http://localhost:$PORT  (Traefik over $INSTANCES instances)"
    echo "  Dashboard:  http://localhost:$PORT/ui/"
    echo "  Health:     http://localhost:$PORT/health"
    echo "  Traefik:    http://localhost:$TRAEFIK_DASHBOARD_PORT/dashboard/"
    echo "  Grafana:    http://localhost:13000"
    echo "  Prometheus: http://localhost:19090"
    echo "  Tempo:      http://localhost:3200"
    print_platform_endpoints
    echo ""
    echo "  Root keypair - the only credential, for the dashboard login, the"
    echo "  TUI, and the admin API:"
    echo "    access key: $ROOT_ACCESS_KEY"
    echo "    secret key: $ROOT_SECRET_KEY"
    echo ""
    echo "    export S3O_ADMIN_ADDR=http://localhost:$PORT"
    echo "    export S3O_ACCESS_KEY_ID=$ROOT_ACCESS_KEY"
    echo "    export S3O_SECRET_ACCESS_KEY=$ROOT_SECRET_KEY"
    echo "    s3-orchestrator admin status"
    echo "    s3-orchestrator tui"
    echo ""
    echo "  Test upload:"
    echo "    aws --endpoint-url http://localhost:$PORT s3 cp /etc/hostname s3://$BUCKET/test.txt"
    echo ""
    echo "  The '$PERF_USER' identity holds $PERF_GRANTS on $BUCKET."
    echo "  Its keypair is in $CREDENTIALS_FILE, and 'make perf' signs as it."
    echo ""
    echo "  Tear down:"
    echo "    $demo_script down"
    echo ""
}
