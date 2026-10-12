#!/bin/bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Local Kubernetes Demo
#
# Author: Alex Freidah
#
# Stands up a complete s3-orchestrator environment in k3d, with every backing
# service running in the cluster: PostgreSQL, Redis and MinIO in services.yaml,
# the observability stack in observability.yaml, both in the "demo"
# namespace. Builds the image from source, installs the production Helm chart
# with the shared demo config, and runs a fleet of pods behind Traefik, the
# way production runs them. The pods find the backing services by cluster DNS
# name. Everything but the scheduler is shared with the Nomad demo through
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
DEMO_NAMESPACE="demo"

# The host ports k3d's load balancer forwards into the cluster: Traefik's,
# then Grafana, Prometheus and Tempo, on the ports print_summary lists.
LB_PORTS=("$PORT" "$TRAEFIK_DASHBOARD_PORT" 13000 19090 3200)

# Force all kubectl and helm commands to target the local k3d cluster, not any
# remote cluster that may be configured in the user's kubeconfig.
kubectl() { command kubectl --context "k3d-${CLUSTER_NAME}" "$@"; }
helm() { command helm --kube-context "k3d-${CLUSTER_NAME}" "$@"; }

cd "$REPO_ROOT"

# print_platform_endpoints lists what only Kubernetes can report: the pods, and
# how to reach one pod's metrics listener, which is not published on the host.
print_platform_endpoints() {
    echo "  Pods:"
    kubectl -n "$NAMESPACE" get pods -l "app.kubernetes.io/name=s3-orchestrator" \
        -o custom-columns='    NAME:.metadata.name,IP:.status.podIP' --no-headers 2>/dev/null || true
    echo "  Metrics and pprof, one listener per pod (scraped by Alloy):"
    echo "    kubectl --context k3d-$CLUSTER_NAME -n $NAMESPACE port-forward pod/<name> 9001"
}

# file_configmap creates or updates a ConfigMap in the demo namespace from
# repository files, so the cluster runs the same deploy/monitoring configs as
# docker-compose. Server-side, so the Grafana dashboard is not also copied
# into a client-side last-applied annotation: at about 150 KB it already
# takes most of that annotation's 256 KB cap.
file_configmap() {
    local name="$1"
    shift
    kubectl -n "$DEMO_NAMESPACE" create configmap "$name" "$@" --dry-run=client -o yaml \
        | kubectl apply --server-side --force-conflicts -f - >/dev/null
}

# wait_for_job_done waits up to the given number of seconds for the Job to
# complete, and returns early with a failure once it has failed, which
# "kubectl wait --for=condition=complete" would sit out to its timeout.
wait_for_job_done() {
    local job="$1" seconds="$2" conditions _
    for _ in $(seq 1 "$seconds"); do
        conditions=$(kubectl -n "$DEMO_NAMESPACE" get job "$job" \
            -o jsonpath='{.status.conditions[?(@.status=="True")].type}' 2>/dev/null || echo "")
        case " $conditions " in
            *" Complete "*) return 0 ;;
            *" Failed "*) return 1 ;;
        esac
        sleep 1
    done
    return 1
}

# check_reused_cluster warns when a cluster created by an older demo lacks a
# host port mapping this one relies on. k3d fixes the mappings at creation.
check_reused_cluster() {
    local port
    for port in "${LB_PORTS[@]}"; do
        if [[ -z "$(docker port "k3d-${CLUSTER_NAME}-serverlb" "$port/tcp" 2>/dev/null)" ]]; then
            echo "Warning: the existing cluster does not forward host port $port. Run"
            echo "         './deploy/kubernetes/local/demo.sh down' and re-run to recreate it."
        fi
    done
}

# --- Teardown ---
if [[ "${1:-}" == "down" ]]; then
    echo "Tearing down demo environment..."
    helm uninstall "$RELEASE" -n "$NAMESPACE" 2>/dev/null || true
    k3d cluster delete "$CLUSTER_NAME" 2>/dev/null || true
    rm -f "$CREDENTIALS_FILE"
    echo "Done."
    exit 0
fi

require_commands docker k3d kubectl helm jq
refuse_running_dev_deps

# --- Create k3d cluster ---
# k3s's bundled Traefik is disabled so the demo runs the same Traefik as the
# Nomad demo. k3d's load balancer publishes the LoadBalancer Services on the
# host ports.
if k3d cluster list | grep -q "$CLUSTER_NAME"; then
    echo "Cluster $CLUSTER_NAME already exists, reusing it."
    check_reused_cluster
else
    echo "Creating k3d cluster..."
    PORT_ARGS=()
    for lb_port in "${LB_PORTS[@]}"; do
        PORT_ARGS+=(-p "$lb_port:$lb_port@loadbalancer")
    done
    k3d cluster create "$CLUSTER_NAME" \
        --k3s-arg "--disable=traefik@server:0" \
        "${PORT_ARGS[@]}"
fi

build_image

echo "Importing image into k3d..."
k3d image import "$IMAGE" -c "$CLUSTER_NAME"

# --- Deploy backing services ---
# The namespace comes with services.yaml, so it is applied first.
echo "Deploying PostgreSQL, Redis and MinIO..."
kubectl apply -f "$SCRIPT_DIR/services.yaml"

echo "Deploying Prometheus, Grafana, Tempo and Loki..."
file_configmap tempo-config --from-file=tempo.yml="$REPO_ROOT/deploy/monitoring/tempo.yml"
file_configmap loki-config --from-file=config.yml="$REPO_ROOT/deploy/monitoring/loki.yml"
file_configmap grafana-datasources \
    --from-file="$REPO_ROOT/deploy/monitoring/grafana/provisioning/datasources/prometheus.yml"
file_configmap grafana-dashboard-providers \
    --from-file="$REPO_ROOT/deploy/monitoring/grafana/provisioning/dashboards/dashboards.yml"
file_configmap grafana-dashboards --from-file="$REPO_ROOT/grafana/s3-orchestrator.json"
kubectl apply -f "$SCRIPT_DIR/observability.yaml"

echo "Waiting for PostgreSQL, Redis and MinIO..."
if ! kubectl -n "$DEMO_NAMESPACE" wait --for=condition=Available --timeout=300s \
    deployment/postgres deployment/redis deployment/minio-1 deployment/minio-2 deployment/minio-3; then
    echo "Error: the backing services did not become ready"
    kubectl -n "$DEMO_NAMESPACE" get pods
    exit 1
fi

echo "Creating the MinIO backend buckets..."
kubectl -n "$DEMO_NAMESPACE" delete job minio-setup --ignore-not-found >/dev/null
kubectl apply -f "$SCRIPT_DIR/minio-setup.yaml"
if ! wait_for_job_done minio-setup 180; then
    echo "Error: the MinIO bucket setup job did not complete"
    kubectl -n "$DEMO_NAMESPACE" logs job/minio-setup --tail=20 || true
    exit 1
fi

# --- Deploy Traefik ---
echo "Deploying Traefik..."
kubectl apply -f "$SCRIPT_DIR/traefik.yaml"

# --- Deploy with Helm ---
echo "Installing s3-orchestrator via Helm ($INSTANCES instances)..."
RENDERED_CONFIG="$(mktemp)"
trap 'rm -f "$RENDERED_CONFIG"' EXIT
DEMO_DNS="$DEMO_NAMESPACE.svc.cluster.local"
render_config "$RENDERED_CONFIG" \
    "POSTGRES_HOST=postgres.$DEMO_DNS" \
    "POSTGRES_PORT=5432" \
    "REDIS_ADDR=redis.$DEMO_DNS:6379" \
    "MINIO_1_ADDR=minio-1.$DEMO_DNS:9000" \
    "MINIO_2_ADDR=minio-2.$DEMO_DNS:9000" \
    "MINIO_3_ADDR=minio-3.$DEMO_DNS:9000" \
    "OTLP_ADDR=tempo.$DEMO_DNS:4317"
helm upgrade --install "$RELEASE" "$REPO_ROOT/deploy/helm/s3-orchestrator" \
    -n "$NAMESPACE" --create-namespace \
    -f "$SCRIPT_DIR/values.yaml" \
    --set "replicaCount=$INSTANCES" \
    --set-file "configFile=$RENDERED_CONFIG"

# --- Deploy monitoring agent ---
kubectl apply -f "$SCRIPT_DIR/alloy-config.yaml"
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
    # The correlation is created through Grafana's API, so the stack has to
    # be up first. It has had the whole fleet rollout to start, so this is
    # normally already true.
    kubectl -n "$DEMO_NAMESPACE" wait --for=condition=Available --timeout=120s \
        deployment/prometheus deployment/tempo deployment/loki deployment/grafana \
        || echo "Warning: continuing without a healthy observability stack."
    create_grafana_correlation
    print_summary "Kubernetes (k3d)" "./deploy/kubernetes/local/demo.sh"
else
    echo "Error: the fleet did not become ready, or Traefik is not routing to it"
    kubectl -n "$NAMESPACE" get pods
    kubectl -n "$NAMESPACE" logs "deployment/$RELEASE" --tail=20 || true
    exit 1
fi
