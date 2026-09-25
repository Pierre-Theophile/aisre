#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Scenario: roll payments.
#
#   deploy/kind/scenarios/rollout.sh                      flip to the other pinned image
#   deploy/kind/scenarios/rollout.sh --image nginx:1.31.4-alpine-slim --version 1.3.9
#   deploy/kind/scenarios/rollout.sh --no-version         image only, leave service.version
#
# This is the change that the rollout-regression fixtures rank first. It moves two things at
# once, because the two feeders see a rollout by two different means (research §11, §12):
#
#   the container image and a change-cause annotation   →  the Kubernetes feeder sees the
#                                                          Deployment revision change and emits
#                                                          a rollout change node with an actor
#   the service.version the emitters report             →  the OTLP feeder sees a service.version
#                                                          transition and emits its own rollout
#
# Idempotent in the only sense that matters for a toggle: running it twice returns payments to
# where it started, and running it while the cluster is missing fails loudly instead of half
# way.

set -euo pipefail

CLUSTER="${KIND_CLUSTER:-sre-agent-demo}"
CONTEXT="${KUBE_CONTEXT:-kind-${CLUSTER}}"
NS="${SHOP_NAMESPACE:-shop}"
TRAFFIC_NS="${TRAFFIC_NAMESPACE:-shop-traffic}"
DEPLOY="payments"

IMAGE_A="nginx:1.31.5-alpine-slim"
IMAGE_B="nginx:1.31.6-alpine-slim"
VERSION_A="1.4.0"
VERSION_B="1.5.0"

WANT_IMAGE=""
WANT_VERSION=""
BUMP_VERSION=1
while [ $# -gt 0 ]; do
  case "$1" in
    --image) WANT_IMAGE="${2:?--image needs a value}"; shift 2 ;;
    --version) WANT_VERSION="${2:?--version needs a value}"; shift 2 ;;
    --no-version) BUMP_VERSION=0; shift ;;
    *) echo "usage: $(basename "$0") [--image IMAGE] [--version VERSION] [--no-version]" >&2; exit 2 ;;
  esac
done

k() { kubectl --context "$CONTEXT" "$@"; }

k get deployment "$DEPLOY" -n "$NS" >/dev/null

CURRENT_IMAGE="$(k get deployment "$DEPLOY" -n "$NS" -o jsonpath='{.spec.template.spec.containers[?(@.name=="app")].image}')"
CURRENT_VERSION="$(k get configmap shop-versions -n "$TRAFFIC_NS" -o jsonpath='{.data.payments}' 2>/dev/null || echo "")"

if [ -z "$WANT_IMAGE" ]; then
  if [ "$CURRENT_IMAGE" = "$IMAGE_B" ]; then WANT_IMAGE="$IMAGE_A"; else WANT_IMAGE="$IMAGE_B"; fi
fi
if [ -z "$WANT_VERSION" ]; then
  if [ "$CURRENT_VERSION" = "$VERSION_B" ]; then WANT_VERSION="$VERSION_A"; else WANT_VERSION="$VERSION_B"; fi
fi

STAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
CAUSE="rollout.sh at $STAMP: $CURRENT_IMAGE -> $WANT_IMAGE"

echo "Rolling $NS/$DEPLOY."
echo "  image    $CURRENT_IMAGE  ->  $WANT_IMAGE"
if [ "$BUMP_VERSION" = "1" ]; then
  echo "  version  ${CURRENT_VERSION:-<unset>}  ->  $WANT_VERSION   (service.version reported by the emitters)"
else
  echo "  version  left at ${CURRENT_VERSION:-<unset>} (--no-version)"
fi
echo "  cause    $CAUSE"
echo

if [ "$CURRENT_IMAGE" = "$WANT_IMAGE" ]; then
  echo "Image is already $WANT_IMAGE; no new revision will be created."
else
  k -n "$NS" set image "deployment/$DEPLOY" "app=$WANT_IMAGE"
  k -n "$NS" annotate deployment "$DEPLOY" "kubernetes.io/change-cause=$CAUSE" --overwrite >/dev/null
  echo "Waiting for the new revision to roll out."
  k -n "$NS" rollout status "deployment/$DEPLOY" --timeout=180s
fi

if [ "$BUMP_VERSION" = "1" ] && k get configmap shop-versions -n "$TRAFFIC_NS" >/dev/null 2>&1; then
  k -n "$TRAFFIC_NS" patch configmap shop-versions --type merge \
    -p "{\"data\":{\"payments\":\"$WANT_VERSION\"}}" >/dev/null
  echo "Patched configmap/shop-versions payments=$WANT_VERSION."
  # The emitters read service.version from the environment, so they have to be restarted for a
  # ConfigMap change to reach the spans.
  k -n "$TRAFFIC_NS" rollout restart deployment/traffic-payments >/dev/null
  k -n "$TRAFFIC_NS" rollout status deployment/traffic-payments --timeout=180s
fi

echo
echo "Done. Both feeders should now carry a rollout change for payments. Give the OTLP feeder"
echo "at least one aggregation window, then:"
echo "  aisre query diff otel.service.name=checkout --from -30m --to now"
