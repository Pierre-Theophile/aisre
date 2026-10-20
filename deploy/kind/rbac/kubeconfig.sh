#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Mints a short-lived token for the sre-agent-feeder ServiceAccount and writes a kubeconfig for
# it to stdout. It refuses when the account can write to anything, or can read Secrets beyond
# what `--secrets metadata` asks for, so a mistake in the role is caught here rather than by a
# feeder that will not start.
#
#   kubectl apply -k deploy/kind/rbac
#   deploy/kind/rbac/kubeconfig.sh > /tmp/sre-agent-feeder.kubeconfig
#   TTL=7200 deploy/kind/rbac/kubeconfig.sh ...   # seconds; the cluster may cap it
set -euo pipefail

namespace=${NAMESPACE:-sre-agent}
account=${ACCOUNT:-sre-agent-feeder}
ttl=${TTL:-3600}
as="system:serviceaccount:${namespace}:${account}"

refuse() { echo "kubeconfig.sh: $*" >&2; exit 1; }

for verb in create update patch delete deletecollection; do
  for resource in deployments.apps statefulsets.apps daemonsets.apps jobs.batch cronjobs.batch \
      services configmaps secrets namespaces nodes ingresses.networking.k8s.io; do
    if kubectl auth can-i "$verb" "$resource" --all-namespaces --as "$as" >/dev/null 2>&1; then
      refuse "$as can $verb $resource; the feeder refuses a credential that can write"
    fi
  done
done
# `get` on secrets is never needed: the metadata-only informer lists and watches.
if kubectl auth can-i get secrets --all-namespaces --as "$as" >/dev/null 2>&1; then
  refuse "$as can get secrets cluster-wide; remove that grant (the feeder never reads a value)"
fi
if kubectl auth can-i list secrets --all-namespaces --as "$as" >/dev/null 2>&1; then
  refuse "$as can list secrets cluster-wide; bind secrets-metadata/ per namespace instead"
fi

server=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
ca=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
cluster=$(kubectl config view --minify -o jsonpath='{.clusters[0].name}')
token=$(kubectl -n "$namespace" create token "$account" --duration "${ttl}s")

cat <<KUBECONFIG
apiVersion: v1
kind: Config
clusters:
  - name: ${cluster}
    cluster:
      server: ${server}
      certificate-authority-data: ${ca}
users:
  - name: ${account}
    user:
      token: ${token}
contexts:
  - name: ${account}@${cluster}
    context:
      cluster: ${cluster}
      user: ${account}
current-context: ${account}@${cluster}
KUBECONFIG
