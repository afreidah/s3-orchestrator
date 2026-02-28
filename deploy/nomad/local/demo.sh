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

# Force all Nomad commands to target the local dev agent, not any remote
# cluster that may be configured in the user's shell environment.
export NOMAD_ADDR="http://127.0.0.1:4646"
unset NOMAD_TOKEN NOMAD_CACERT NOMAD_CLIENT_CERT NOMAD_CLIENT_KEY NOMAD_TLS_SERVER_NAME NOMAD_NAMESPACE NOMAD_REGION

cd "$REPO_ROOT"

# --- Teardown ---
if [[ "${1:-}" == "down" ]]; then
    echo "Tearing down demo environment..."
    nomad job stop -purge s3-orchestrator 2>/dev/null || true
    if [[ -f /tmp/nomad-demo.pid ]]; then
        kill "$(cat /tmp/nomad-demo.pid)" 2>/dev/null || true
        rm -f /tmp/nomad-demo.pid
    fi
    docker compose -f docker-compose.test.yml down 2>/dev/null || true
    echo "Done."
    exit 0
fi

# --- Preflight checks ---
for cmd in docker nomad; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Error: $cmd is required but not installed."
        exit 1
    fi
done

# --- Start backing services ---
echo "Starting PostgreSQL and MinIO via docker-compose..."
docker compose -f docker-compose.test.yml up -d --wait postgres minio-1 minio-2
docker compose -f docker-compose.test.yml up -d minio-setup

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

# --- Submit job ---
echo "Submitting Nomad job..."
sed "s/__HOST_IP__/$HOST_IP/g" "$SCRIPT_DIR/s3-orchestrator.nomad.hcl" | nomad job run -detach -

# --- Wait for healthy allocation ---
echo "Waiting for allocation to become healthy..."
for i in $(seq 1 60); do
    STATUS=$(nomad job status -short s3-orchestrator 2>/dev/null | grep -c "running" || true)
    if [[ "$STATUS" -ge 1 ]]; then
        HEALTH=$(curl -s "http://localhost:$PORT/health" 2>/dev/null || true)
        if [[ "$HEALTH" == "ok" ]]; then
            break
        fi
    fi
    sleep 1
done

HEALTH=$(curl -s "http://localhost:$PORT/health" 2>/dev/null || true)
if [[ "$HEALTH" == "ok" ]]; then
    echo ""
    echo "========================================"
    echo "  S3 Orchestrator is running in Nomad"
    echo "========================================"
    echo ""
    echo "  S3 API:     http://localhost:$PORT"
    echo "  Dashboard:  http://localhost:$PORT/ui/"
    echo "  Metrics:    http://localhost:$PORT/metrics"
    echo "  Health:     http://localhost:$PORT/health"
    echo "  Nomad UI:   http://localhost:4646"
    echo ""
    echo "  Dashboard login: admin / admin"
    echo ""
    echo "  Test upload:"
    echo "    aws --endpoint-url http://localhost:$PORT s3 cp /etc/hostname s3://photos/test.txt"
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
