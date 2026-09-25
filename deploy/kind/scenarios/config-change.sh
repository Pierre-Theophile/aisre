#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Scenario: change checkout's configuration without deploying anything.
#
#   deploy/kind/scenarios/config-change.sh                    toggle CART_TTL_SECONDS
#   deploy/kind/scenarios/config-change.sh --key FEATURE_FLAGS --value express-checkout,gift-wrap
#
# This is the config-change fixture (T066): the ConfigMap moves, nothing else does. The
# Kubernetes feeder sees the ConfigMap's resourceVersion change and emits a config_change change
# node plus a new CONFIG node version carrying a new value hash (research §12) — and, crucially,
# no rollout, because the pods are deliberately NOT restarted. A restart here would put a
# rollout change node next to the config change and make the fixture ambiguous.
#
# Idempotent as a toggle: the default flips CART_TTL_SECONDS between two values.

set -euo pipefail

CLUSTER="${KIND_CLUSTER:-sre-agent-demo}"
CONTEXT="${KUBE_CONTEXT:-kind-${CLUSTER}}"
NS="${SHOP_NAMESPACE:-shop}"
CM="checkout-config"

KEY="CART_TTL_SECONDS"
VALUE=""
VALUE_A="900"
VALUE_B="1800"

while [ $# -gt 0 ]; do
  case "$1" in
    --key) KEY="${2:?--key needs a value}"; shift 2 ;;
    --value) VALUE="${2:?--value needs a value}"; shift 2 ;;
    *) echo "usage: $(basename "$0") [--key KEY] [--value VALUE]" >&2; exit 2 ;;
  esac
done

k() { kubectl --context "$CONTEXT" "$@"; }

k get configmap "$CM" -n "$NS" >/dev/null

BEFORE_RV="$(k get configmap "$CM" -n "$NS" -o jsonpath='{.metadata.resourceVersion}')"
BEFORE_VAL="$(k get configmap "$CM" -n "$NS" -o "jsonpath={.data.$KEY}" 2>/dev/null || echo "")"

if [ -z "$VALUE" ]; then
  if [ "$KEY" != "CART_TTL_SECONDS" ]; then
    echo "--value is required for any key other than CART_TTL_SECONDS" >&2
    exit 2
  fi
  if [ "$BEFORE_VAL" = "$VALUE_B" ]; then VALUE="$VALUE_A"; else VALUE="$VALUE_B"; fi
fi

echo "Changing configmap/$CM in $NS."
echo "  key              $KEY"
echo "  value            ${BEFORE_VAL:-<unset>}  ->  $VALUE"
echo "  resourceVersion  $BEFORE_RV  (a new one is what the feeder keys the change on)"
echo "  pods             not restarted, on purpose: this scenario is a config change and"
echo "                   nothing else"
echo

if [ "$BEFORE_VAL" = "$VALUE" ]; then
  echo "Already $VALUE. Writing it anyway would not bump resourceVersion, so nothing is done."
  exit 0
fi

k -n "$NS" patch configmap "$CM" --type merge -p "{\"data\":{\"$KEY\":\"$VALUE\"}}" >/dev/null

AFTER_RV="$(k get configmap "$CM" -n "$NS" -o jsonpath='{.metadata.resourceVersion}')"
echo "Patched. resourceVersion $BEFORE_RV -> $AFTER_RV."
echo
echo "The Kubernetes feeder should emit a config_change for $NS/$CM and a new CONFIG version."
echo "  aisre query diff otel.service.name=checkout --from -30m --to now"
