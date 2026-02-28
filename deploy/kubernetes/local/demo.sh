#!/bin/bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Local Kubernetes Demo
#
# Author: Alex Freidah
#
# Stands up a complete s3-orchestrator environment in k3d with PostgreSQL and
# MinIO backends running via docker-compose on the host. Builds the image from
# source, imports it into the cluster, and applies all manifests. Tears down
# cleanly with the "down" argument.
#
# Usage:
#   ./demo.sh        # stand up the full environment
#   ./demo.sh down   # tear everything down
# -------------------------------------------------------------------------------

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
CLUSTER_NAME="s3-orchestrator-demo"
NAMESPACE="s3-orchestrator"
IMAGE="s3-orchestrator:local"
PORT=9000

cd "$REPO_ROOT"

# --- Teardown ---
if [[ "${1:-}" == "down" ]]; then
    echo "Tearing down demo environment..."
    k3d cluster delete "$CLUSTER_NAME" 2>/dev/null || true
    docker compose -f docker-compose.test.yml down 2>/dev/null || true
    echo "Done."
    exit 0
fi

# --- Preflight checks ---
for cmd in docker k3d kubectl; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Error: $cmd is required but not installed."
        exit 1
    fi
done

# --- Start backing services ---
echo "Starting PostgreSQL and MinIO via docker-compose..."
docker compose -f docker-compose.test.yml up -d --wait postgres minio-1 minio-2
docker compose -f docker-compose.test.yml up -d minio-setup

# --- Create k3d cluster ---
if k3d cluster list | grep -q "$CLUSTER_NAME"; then
    echo "Cluster $CLUSTER_NAME already exists, reusing it."
else
    echo "Creating k3d cluster (Traefik disabled)..."
    k3d cluster create "$CLUSTER_NAME" \
        --k3s-arg "--disable=traefik@server:0"
fi

# --- Build and import image ---
echo "Building container image..."
docker build -t "$IMAGE" .

echo "Importing image into k3d..."
k3d image import "$IMAGE" -c "$CLUSTER_NAME"

# --- Discover host gateway IP ---
HOST_IP=$(docker network inspect "k3d-${CLUSTER_NAME}" \
    -f '{{(index .IPAM.Config 0).Gateway}}')
echo "Host gateway IP: $HOST_IP"

# --- Apply manifests ---
echo "Applying Kubernetes manifests..."
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -f deploy/kubernetes/service.yaml
kubectl apply -f deploy/kubernetes/local/secret.yaml
sed "s/__HOST_IP__/$HOST_IP/g" deploy/kubernetes/local/configmap.yaml | kubectl apply -f -
kubectl apply -f deploy/kubernetes/local/deployment.yaml

# --- Wait for rollout ---
echo "Waiting for deployment to roll out..."
kubectl -n "$NAMESPACE" rollout status deployment/s3-orchestrator --timeout=60s

# --- Verify health ---
echo "Starting port-forward on localhost:$PORT..."
kubectl -n "$NAMESPACE" port-forward svc/s3-orchestrator "$PORT":9000 &
PF_PID=$!
trap "kill $PF_PID 2>/dev/null" EXIT

sleep 2

HEALTH=$(curl -s "http://localhost:$PORT/health")
if [[ "$HEALTH" == "ok" ]]; then
    echo ""
    echo "========================================"
    echo "  S3 Orchestrator is running in k3d"
    echo "========================================"
    echo ""
    echo "  S3 API:     http://localhost:$PORT"
    echo "  Dashboard:  http://localhost:$PORT/ui/"
    echo "  Metrics:    http://localhost:$PORT/metrics"
    echo "  Health:     http://localhost:$PORT/health"
    echo ""
    echo "  Dashboard login: admin / admin"
    echo ""
    echo "  Test upload:"
    echo "    aws --endpoint-url http://localhost:$PORT s3 cp /etc/hostname s3://photos/test.txt"
    echo ""
    echo "  Tear down:"
    echo "    ./deploy/kubernetes/local/demo.sh down"
    echo ""
    echo "Port-forward is running in the foreground. Press Ctrl+C to stop."
    wait $PF_PID
else
    echo "Error: health check returned '$HEALTH' (expected 'ok')"
    kubectl -n "$NAMESPACE" logs deployment/s3-orchestrator --tail=20
    exit 1
fi
