#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Delete the disposable cluster. Idempotent, and it deletes the cluster, not the workloads:
# there is nothing here worth keeping.
#
#   deploy/kind/down.sh

set -euo pipefail

CLUSTER="${KIND_CLUSTER:-sre-agent-demo}"

command -v kind >/dev/null 2>&1 || { echo "kind is not on PATH" >&2; exit 1; }

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "Deleting kind cluster $CLUSTER (and its kubeconfig context kind-$CLUSTER)."
  kind delete cluster --name "$CLUSTER"
  echo "Gone."
else
  echo "No kind cluster named $CLUSTER; nothing to delete."
fi

echo
echo "Recordings under /tmp/live (or wherever --record pointed) are untouched."
