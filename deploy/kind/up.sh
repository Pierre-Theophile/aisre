#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Create the disposable cluster and deploy one overlay. Idempotent: run it as often as you like.
#
#   deploy/kind/up.sh                 cluster + the minimal shop
#   deploy/kind/up.sh full            cluster + the full OpenTelemetry Demo
#   deploy/kind/up.sh none            cluster only
#
# Everything it does is also spelled out command by command in deploy/kind/README.md; this is
# the short way.

set -euo pipefail

OVERLAY="${1:-minimal}"
CLUSTER="${KIND_CLUSTER:-sre-agent-demo}"
CONTEXT="${KUBE_CONTEXT:-kind-${CLUSTER}}"
HERE="$(cd "$(dirname "$0")" && pwd)"

case "$OVERLAY" in
  minimal|full|none) ;;
  *) echo "usage: $(basename "$0") [minimal|full|none]" >&2; exit 2 ;;
esac

for tool in kind kubectl docker; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is not on PATH" >&2; exit 1; }
done

if ! docker info >/dev/null 2>&1; then
  echo "Docker is not running. kind needs it; start Docker and try again." >&2
  exit 1
fi

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "Cluster $CLUSTER already exists; leaving it alone."
else
  echo "Creating kind cluster $CLUSTER from $HERE/cluster.yaml."
  kind create cluster --config "$HERE/cluster.yaml" --name "$CLUSTER" --wait 120s
fi

echo
echo "Nodes and their pool labels:"
kubectl --context "$CONTEXT" get nodes \
  -o custom-columns='NAME:.metadata.name,POOL:.metadata.labels.sre\.node_pool'

echo
echo "Writing the host OTLP endpoint into the overlays."
"$HERE/host-endpoint.sh" >/dev/null
"$HERE/host-endpoint.sh" --print

if [ "$OVERLAY" = "none" ]; then
  echo
  echo "Cluster only, as asked. Deploy with:  kubectl apply -k $HERE/otel-demo/minimal --context $CONTEXT"
  exit 0
fi

if [ "$OVERLAY" = "full" ]; then
  echo
  echo "Making sure the upstream OpenTelemetry Demo manifest is present."
  "$HERE/otel-demo/fetch-upstream.sh"
fi

echo
echo "Applying the $OVERLAY overlay."
kubectl apply -k "$HERE/otel-demo/$OVERLAY" --context "$CONTEXT"

echo
echo "Waiting for the workloads to come up (this pulls images the first time)."
if [ "$OVERLAY" = "minimal" ]; then
  kubectl --context "$CONTEXT" -n shop rollout status deployment/storefront --timeout=180s
  kubectl --context "$CONTEXT" -n shop rollout status deployment/checkout --timeout=180s
  kubectl --context "$CONTEXT" -n shop rollout status deployment/payments --timeout=180s
  kubectl --context "$CONTEXT" -n shop rollout status deployment/inventory --timeout=180s
  kubectl --context "$CONTEXT" -n shop rollout status deployment/payments-db --timeout=180s
  kubectl --context "$CONTEXT" -n shop rollout status statefulset/redis --timeout=180s
  kubectl --context "$CONTEXT" -n shop-traffic rollout status deployment/traffic-checkout --timeout=180s
else
  kubectl --context "$CONTEXT" -n otel-demo rollout status deployment/otel-collector --timeout=300s
fi

echo
echo "Up. Next:"
echo "  aisre feed otel --source-id=otel:kind --listen-grpc=:4317 --listen-http=:4318 --window=5m --record=/tmp/live/otel"
echo "  aisre feed k8s  --source-id=k8s:kind --context=$CONTEXT --owner-labels=team --record=/tmp/live/k8s"
echo "  deploy/kind/scenarios/rollout.sh"
echo "  deploy/kind/down.sh"
