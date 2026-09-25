#!/bin/bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Local Kubernetes Demo
#
# Author: Alex Freidah
#
# Stands up a complete s3-orchestrator environment in k3d with PostgreSQL,
# Redis and MinIO backends running via docker-compose on the host. Builds the
# image from source, installs the production Helm chart with the shared demo
# config, and runs a fleet of pods behind Traefik, the way production runs
# them. Everything but the scheduler is shared with the Nomad demo through
# deploy/local. Tears down cleanly with "down".
#
# Usage:
#   ./demo.sh                # stand up the full environment
#   INSTANCES=1 ./demo.sh    # a single pod, still behind Traefik
#   ./demo.sh down           # tear everything down
# -------------------------------------------------------------------------------

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=SCRIPTDIR/../../local/lib.sh
source "$SCRIPT_DIR/../../local/lib.sh"

CLUSTER_NAME="s3-orchestrator-demo"
NAMESPACE="s3-orchestrator"
RELEASE="s3-orchestrator"

# Force all kubectl and helm commands to target the local k3d cluster, not any
# remote cluster that may be configured in the user's kubeconfig.
kubectl() { command kubectl --context "k3d-${CLUSTER_NAME}" "$@"; }
helm() { command helm --kube-context "k3d-${CLUSTER_NAME}" "$@"; }

cd "$REPO_ROOT"

# reload_fleet sends SIGHUP to every orchestrator pod so each rebuilds its
# credential registry from the store. Provisioning only republishes the registry
# on the instance that served the call, so without this the perf identity
# authenticates on one pod and gets 403 from the rest.
reload_fleet() {
    echo "Reloading every instance so it picks up the perf identity..."
    local pod
    for pod in $(kubectl -n "$NAMESPACE" get pods \
        -l "app.kubernetes.io/name=s3-orchestrator" \
        --field-selector=status.phase=Running -o name); do
        kubectl -n "$NAMESPACE" exec "$pod" -- kill -HUP 1 >/dev/null 2>&1 \
            || echo "Warning: could not signal ${pod#pod/}"
    done
    sleep 2
}

# print_platform_endpoints lists what only Kubernetes can report: the pods, and
# how to reach one pod's metrics listener, which is not published on the host.
print_platform_endpoints() {
    echo "  Pods:"
    kubectl -n "$NAMESPACE" get pods -l "app.kubernetes.io/name=s3-orchestrator" \
        -o custom-columns='    NAME:.metadata.name,IP:.status.podIP' --no-headers 2>/dev/null || true
    echo "  Metrics and pprof, one listener per pod (scraped by Alloy):"
    echo "    kubectl --context k3d-$CLUSTER_NAME -n $NAMESPACE port-forward pod/<name> 9001"
}

# --- Teardown ---
if [[ "${1:-}" == "down" ]]; then
    echo "Tearing down demo environment..."
    helm uninstall "$RELEASE" -n "$NAMESPACE" 2>/dev/null || true
    k3d cluster delete "$CLUSTER_NAME" 2>/dev/null || true
    rm -f "$CREDENTIALS_FILE"
    stop_backing_services
    echo "Done."
    exit 0
fi

require_commands docker k3d kubectl helm jq
start_backing_services

# --- Create k3d cluster ---
# k3s's bundled Traefik is disabled so the demo runs the same Traefik as the
# Nomad demo. k3d's load balancer publishes its Service on the host ports.
if k3d cluster list | grep -q "$CLUSTER_NAME"; then
    echo "Cluster $CLUSTER_NAME already exists, reusing it."
else
    echo "Creating k3d cluster..."
    k3d cluster create "$CLUSTER_NAME" \
        --k3s-arg "--disable=traefik@server:0" \
        -p "$PORT:$PORT@loadbalancer" \
        -p "$TRAEFIK_DASHBOARD_PORT:$TRAEFIK_DASHBOARD_PORT@loadbalancer"
fi

build_image

echo "Importing image into k3d..."
k3d image import "$IMAGE" -c "$CLUSTER_NAME"

# --- Discover host gateway IP ---
HOST_IP=$(docker network inspect "k3d-${CLUSTER_NAME}" \
    -f '{{(index .IPAM.Config 0).Gateway}}')
echo "Host gateway IP: $HOST_IP"

# Alloy runs in the cluster here, so compose starts everything else.
docker compose -f "$COMPOSE_FILE" up -d tempo loki prometheus grafana

# --- Deploy Traefik ---
echo "Deploying Traefik..."
kubectl apply -f "$SCRIPT_DIR/traefik.yaml"

# --- Deploy with Helm ---
echo "Installing s3-orchestrator via Helm ($INSTANCES instances)..."
RENDERED_CONFIG="$(mktemp)"
trap 'rm -f "$RENDERED_CONFIG"' EXIT
render_config "$HOST_IP" "$RENDERED_CONFIG"
helm upgrade --install "$RELEASE" "$REPO_ROOT/deploy/helm/s3-orchestrator" \
    -n "$NAMESPACE" --create-namespace \
    -f "$SCRIPT_DIR/values.yaml" \
    --set "replicaCount=$INSTANCES" \
    --set-file "configFile=$RENDERED_CONFIG"

# --- Deploy monitoring agent ---
sed "s/__HOST_IP__/$HOST_IP/g" "$SCRIPT_DIR/alloy-config.yaml" | kubectl apply -f -
kubectl apply -f "$SCRIPT_DIR/alloy-daemonset.yaml"

# --- Wait for a healthy fleet ---
# Every pod must be ready before traffic starts: the perf suite measures the
# whole fleet, and a run that began against one pod while the others were
# still booting would measure something else.
echo "Waiting for $INSTANCES ready pods..."
ROLLED_OUT=true
kubectl -n traefik rollout status deployment/traefik --timeout=120s || ROLLED_OUT=false
kubectl -n "$NAMESPACE" rollout status "deployment/$RELEASE" --timeout=180s || ROLLED_OUT=false

echo "Waiting for Traefik to route to the fleet..."
if [[ "$ROLLED_OUT" == "true" ]] && wait_for_health 30; then
    provision_perf_identity
    reload_fleet
    create_grafana_correlation
    print_summary "Kubernetes (k3d)" "./deploy/kubernetes/local/demo.sh"
else
    echo "Error: the fleet did not become ready, or Traefik is not routing to it"
    kubectl -n "$NAMESPACE" get pods
    kubectl -n "$NAMESPACE" logs "deployment/$RELEASE" --tail=20 || true
    exit 1
fi
