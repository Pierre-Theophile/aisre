<!-- SPDX-License-Identifier: Apache-2.0 -->

# A read-only credential for `feed k8s`

`aisre feed k8s` asks the API server what its credential may do — a `SelfSubjectRulesReview`,
the same question `kubectl auth can-i --list` asks — before it emits a single event, and
**refuses to start** if the answer includes `create`, `update`, `patch`, `delete` or
`deletecollection` on any resource it watches. That is FR-046 and constitution VII: *no component
writes to any production system*, and the only honest way to state that is to ask the system
rather than to promise it in a comment.

kind's default kubeconfig is `cluster-admin`, so it fails that check. Correctly. This directory
is the credential that passes it.

```sh
kubectl apply -k deploy/kind/rbac
deploy/kind/rbac/kubeconfig.sh > /tmp/sre-agent-feeder.kubeconfig

aisre feed k8s \
  --kubeconfig /tmp/sre-agent-feeder.kubeconfig \
  --source-id k8s:demo --cluster-name shop-prod --namespaces shop
```

## What is here

| File | What it is |
|---|---|
| `namespace.yaml` | `sre-agent`, which holds the identity and nothing else |
| `serviceaccount.yaml` | `sre-agent-feeder`, the identity the token belongs to |
| `clusterrole.yaml` | `get,list,watch` on exactly the resources the informers read, plus `create` on `selfsubjectrulesreviews` |
| `clusterrolebinding.yaml` | binds the two, cluster-wide (Nodes and Namespaces are cluster-scoped) |
| `kubeconfig.sh` | mints a short-lived token through the TokenRequest API and writes a kubeconfig to stdout |

The resource list in `clusterrole.yaml` is the same one
`internal/feeders/k8s/permissions.go` checks a credential against, and
`TestRequiredScopesCoverEveryWatchedResource` fails if the informers grow a resource the
documented scopes do not mention.

**Pods are deliberately absent.** The feeder does not watch them — they churn on every rollout
and answer nothing the graph is asked at that granularity (research §12) — so it does not ask to
read them, which keeps every container's environment out of reach of this credential.

## The token

`kubeconfig.sh` uses `kubectl create token`, so the credential is short-lived (an hour by
default) and is never written into a Secret that lives in the cluster. Re-run the script when it
expires; `TTL=7200 deploy/kind/rbac/kubeconfig.sh` asks for longer, up to whatever the cluster
allows.

Before it prints anything, the script asks `kubectl auth can-i` the same five write verbs the
feeder asks about, impersonating the ServiceAccount. If any of them come back allowed it refuses
to hand you the token, so a mistake in the ClusterRole is caught here rather than by a feeder
that will not start.

## `--allow-write-credentials`

There is an escape hatch, and it is a bad idea:

```sh
aisre feed k8s --allow-write-credentials ...   # logs a loud warning, every time
```

It exists for the ten minutes before you have read this page, on a disposable cluster you are
about to delete. Constitution VII says a connector that requires write scope MUST be rejected;
the flag does not make that untrue, it only lets a developer get on with the demo. Against a
cluster set up with `kubectl apply -k deploy/kind/rbac` it is never needed, which is the point of
this directory existing.

## Against a real cluster

Two changes, both narrowing:

- Replace the `ClusterRoleBinding` with a `ClusterRoleBinding` covering only `nodes` and
  `namespaces` and a `RoleBinding` per watched namespace for everything else, so the grant is as
  narrow as `--namespaces` is.
- If the feeder runs as a pod rather than on a host, drop `kubeconfig.sh` entirely: give the pod
  the `sre-agent-feeder` ServiceAccount and let `feed k8s` pick up the in-cluster credential,
  which it does when no kubeconfig is found.

Reading Secrets is still reading Secrets. The feeder replaces every value with a digest of itself
before the bytes reach a payload (`informers.go`, `RedactSecret`), and a `CONFIG` node for a
Secret carries its version and nothing else, so no material reaches the graph — but the
credential can still read them, and an operator who would rather it could not should remove
`secrets` from the ClusterRole and accept that `depends-on` edges to Secrets will point at
placeholders.

**Recordings needed more than digesting the values, and until 2026-09-16 they did not get it.**
`kubectl apply` stores the entire submitted manifest — `stringData` included, in cleartext — in
the annotation `kubectl.kubernetes.io/last-applied-configuration`. `RedactSecret` copied
metadata untouched, so `--record` against any cluster that had ever been `kubectl apply`-ed
wrote the Secret's own value to a file (live run 2026-09-16, finding 5). `RedactSecret` now
drops that annotation, drops any annotation whose value is a JSON manifest carrying `data` or
`stringData`, drops `managedFields`, and keeps only the metadata the mappers read. Every other
kind loses the same annotation and the `fieldsV1` half of `managedFields`
(`SanitizeMetadata`). `TestNoSecretMaterialInTheCorpus` checks the committed corpus and the
shipped fixtures' `payloads/secrets/` for the demo Secret's value and for that annotation, so a
regression is caught by the recording rather than by a reader of a public repository.
