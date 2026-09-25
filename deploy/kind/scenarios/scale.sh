#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Scenario: scale a workload.
#
#   deploy/kind/scenarios/scale.sh                            toggle inventory between 3 and 5
#   deploy/kind/scenarios/scale.sh --deployment checkout --replicas 1
#
# A replica change and nothing else: no new pod template, so no rollout. The Kubernetes feeder
# emits a `scaling` change node and a new WORKLOAD version with a different sre.k8s.replicas
# (FR-043).

set -euo pipefail

CLUSTER="${KIND_CLUSTER:-sre-agent-demo}"
CONTEXT="${KUBE_CONTEXT:-kind-${CLUSTER}}"
NS="${SHOP_NAMESPACE:-shop}"

DEPLOY="inventory"
REPLICAS=""
REPLICAS_A="3"
REPLICAS_B="5"

while [ $# -gt 0 ]; do
  case "$1" in
    --deployment) DEPLOY="${2:?--deployment needs a value}"; shift 2 ;;
    --replicas) REPLICAS="${2:?--replicas needs a value}"; shift 2 ;;
    *) echo "usage: $(basename "$0") [--deployment NAME] [--replicas N]" >&2; exit 2 ;;
  esac
done

k() { kubectl --context "$CONTEXT" "$@"; }

k get deployment "$DEPLOY" -n "$NS" >/dev/null

CURRENT="$(k get deployment "$DEPLOY" -n "$NS" -o jsonpath='{.spec.replicas}')"
if [ -z "$REPLICAS" ]; then
  if [ "$CURRENT" = "$REPLICAS_B" ]; then REPLICAS="$REPLICAS_A"; else REPLICAS="$REPLICAS_B"; fi
fi

echo "Scaling $NS/$DEPLOY."
echo "  replicas  $CURRENT  ->  $REPLICAS"
echo

if [ "$CURRENT" = "$REPLICAS" ]; then
  echo "Already at $REPLICAS replicas; nothing to do."
  exit 0
fi

k -n "$NS" scale "deployment/$DEPLOY" --replicas="$REPLICAS"
k -n "$NS" rollout status "deployment/$DEPLOY" --timeout=180s

echo
echo "Done. Expect one scaling change node for $NS/$DEPLOY and no rollout."
