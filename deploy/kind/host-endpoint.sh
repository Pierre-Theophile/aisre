#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Work out the address that pods in the kind cluster can use to reach a process listening on
# the host, and write it into both overlays' sre-agent-otlp ConfigMap.
#
#   deploy/kind/host-endpoint.sh            detect, write the overlays, apply + roll if the
#                                           cluster is up
#   deploy/kind/host-endpoint.sh --print    print the address and do nothing else
#   deploy/kind/host-endpoint.sh --host 10.0.0.7   use this address instead of detecting one
#
# Why this exists
# ---------------
# `aisre feed otel` listens on the host (:4317 gRPC, :4318 HTTP). The pods have to reach
# out to it. kind's extraPortMappings are the other direction — host into the cluster — so they
# are no help here. The address that works depends on how Docker is running:
#
#   Docker Desktop (macOS, Windows)   host.docker.internal, injected into every container's
#                                     /etc/hosts, pointing at an address the VM routes to the
#                                     host.
#   Docker Engine on Linux            the gateway of the `kind` bridge network, usually
#                                     172.18.0.1.
#
# And the name cannot simply be used as-is: kind rewrites each node's /etc/resolv.conf away
# from Docker's embedded resolver (127.0.0.11 would be the pod's own loopback), so
# host.docker.internal does not resolve inside a pod. So the name is resolved here, on the
# host side, from inside the node container, and the resulting literal is what the pods get.
#
# Idempotent: running it twice writes the same bytes and rolls nothing that is already correct.

set -euo pipefail

CLUSTER="${KIND_CLUSTER:-sre-agent-demo}"
CONTEXT="${KUBE_CONTEXT:-kind-${CLUSTER}}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OVERLAYS=("$HERE/otel-demo/minimal/host-endpoint.yaml" "$HERE/otel-demo/full/host-endpoint.yaml")
NODE="${CLUSTER}-control-plane"

MODE="apply"
HOST_OVERRIDE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --print) MODE="print"; shift ;;
    --host)
      HOST_OVERRIDE="${2:-}"
      [ -n "$HOST_OVERRIDE" ] || { echo "--host needs a value" >&2; exit 2; }
      shift 2 ;;
    *) echo "usage: $(basename "$0") [--print] [--host ADDRESS]" >&2; exit 2 ;;
  esac
done

# Echoes "ADDRESS|how it was found".
detect_host() {
  local ip gw
  # 1. Docker Desktop: resolve host.docker.internal from inside the node container.
  if command -v docker >/dev/null 2>&1 && docker inspect "$NODE" >/dev/null 2>&1; then
    ip="$(docker exec "$NODE" getent hosts host.docker.internal 2>/dev/null \
          | awk 'NR==1{print $1}' || true)"
    if [ -n "$ip" ]; then
      echo "$ip|host.docker.internal, resolved inside $NODE"
      return 0
    fi
  fi
  # 2. Docker Engine on Linux: the gateway of the network kind put the nodes on.
  if command -v docker >/dev/null 2>&1; then
    gw="$(docker network inspect kind \
          --format '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}}{{end}}{{end}}' 2>/dev/null || true)"
    if [ -n "$gw" ]; then
      echo "$gw|gateway of the kind Docker network"
      return 0
    fi
  fi
  # 3. Nothing to go on: neither a running node nor a kind network. The committed default is
  #    the address Docker Engine on Linux almost always gives that network.
  echo "172.18.0.1|fallback, no running cluster and no kind network to inspect"
  return 0
}

if [ -n "$HOST_OVERRIDE" ]; then
  HOST="$HOST_OVERRIDE"
  SOURCE="given on the command line"
else
  DETECTED="$(detect_host)"
  HOST="${DETECTED%%|*}"
  SOURCE="${DETECTED#*|}"
fi

echo "Host address reachable from inside the cluster: $HOST"
echo "  source   $SOURCE"
echo "  OTLP     gRPC $HOST:4317   HTTP http://$HOST:4318"

if [ "$MODE" = "print" ]; then
  exit 0
fi

echo
echo "Writing it into the overlays."
for f in "${OVERLAYS[@]}"; do
  if [ ! -f "$f" ]; then
    echo "  skip     $f (not found)"
    continue
  fi
  before="$(grep -E '^  OTLP_HOST:' "$f" || true)"
  tmp="$f.tmp"
  sed -e "s|^  OTLP_HOST: .*|  OTLP_HOST: \"$HOST\"|" \
      -e "s|^  OTLP_GRPC_ENDPOINT: .*|  OTLP_GRPC_ENDPOINT: \"$HOST:4317\"|" \
      -e "s|^  OTLP_HTTP_ENDPOINT: .*|  OTLP_HTTP_ENDPOINT: \"http://$HOST:4318\"|" \
      "$f" > "$tmp"
  if cmp -s "$f" "$tmp"; then
    rm -f "$tmp"
    echo "  same     $f"
  else
    mv "$tmp" "$f"
    echo "  wrote    $f  (was${before#*OTLP_HOST:})"
  fi
done

if ! kubectl --context "$CONTEXT" version >/dev/null 2>&1; then
  echo
  echo "No reachable cluster on context $CONTEXT; the files are written, apply them when it is up:"
  echo "  kubectl apply -k $HERE/otel-demo/minimal --context $CONTEXT"
  exit 0
fi

echo
echo "Applying the ConfigMaps that exist in the cluster and restarting their readers."
for ns in shop-traffic otel-demo; do
  if ! kubectl --context "$CONTEXT" get namespace "$ns" >/dev/null 2>&1; then
    continue
  fi
  kubectl --context "$CONTEXT" -n "$ns" create configmap sre-agent-otlp \
    --from-literal=OTLP_HOST="$HOST" \
    --from-literal=OTLP_GRPC_ENDPOINT="$HOST:4317" \
    --from-literal=OTLP_HTTP_ENDPOINT="http://$HOST:4318" \
    --dry-run=client -o yaml | kubectl --context "$CONTEXT" apply -f - >/dev/null
  echo "  applied  configmap/sre-agent-otlp in $ns"
  # Nothing re-reads a ConfigMap that is consumed as environment variables, so roll whatever
  # reads it.
  if [ "$ns" = "shop-traffic" ]; then
    kubectl --context "$CONTEXT" -n "$ns" rollout restart deployment \
      -l sre.role=traffic-generator >/dev/null
    echo "  rolled   the telemetrygen emitters in $ns"
  else
    kubectl --context "$CONTEXT" -n "$ns" rollout restart \
      deployment/otel-collector >/dev/null 2>&1 &&
      echo "  rolled   deployment/otel-collector in $ns" || true
  fi
done

echo
echo "Point the feeder at the host side of that address:"
echo "  aisre feed otel --source-id=otel:kind --listen-grpc=:4317 --listen-http=:4318"
